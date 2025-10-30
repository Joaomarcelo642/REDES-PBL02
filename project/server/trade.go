package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	tradeQueueKey = "trade_queue"
	tradeLockKey  = "lock:trade"
)

type TradeTicket struct {
	PlayerName string `json:"player_name"`
	ServerID   string `json:"server_id"`
	Card       Card   `json:"card"`
}

// handleTradeCard é chamado pelo websocket.go
func (s *Server) handleTradeCard(player *PlayerState, command string) {
	// 1. Validar o estado do jogador
	player.mu.Lock()
	if player.State == "InGame" || player.State == "Searching" {
		player.mu.Unlock()
		s.sendWebSocketMessage(player, "Você não pode trocar cartas enquanto estiver em jogo ou procurando partida.")
		return
	}
	player.mu.Unlock()

	// 2. Parsear o índice
	indexStr := strings.TrimSpace(strings.TrimPrefix(command, "TRADE_CARD"))
	if indexStr == "" {
		s.sendWebSocketMessage(player, "Comando inválido. Use 'TRADE_CARD [numero]'.")
		return
	}

	index, err := strconv.Atoi(indexStr)
	if err != nil {
		s.sendWebSocketMessage(player, "Número da carta inválido.")
		return
	}

	if index < 1 || index > len(player.Deck) {
		s.sendWebSocketMessage(player, "Número da carta fora do alcance do seu deck.")
		return
	}

	cardIndex := index - 1

	// 3. Remover a carta do deck do jogador (localmente)
	cardToTrade := player.Deck[cardIndex]
	player.Deck = append(player.Deck[:cardIndex], player.Deck[cardIndex+1:]...)

	log.Printf("Jogador %s está tentando trocar a carta: %s", player.Name, cardToTrade.Name)

	// 4. Executar a troca distribuída
	s.performDistributedTrade(player, cardToTrade)
}

// performDistributedTrade usa TradeTicket e Pub/Sub para notificar o remetente.
func (s *Server) performDistributedTrade(player *PlayerState, cardToTrade Card) {
	ctx := context.Background()

	// 1. Tenta adquirir um lock distribuído
	lockValue := fmt.Sprintf("%s-%d", s.ServerID, time.Now().UnixNano())
	lockTimeout := 3 * time.Second

	ok, err := s.RedisClient.SetNX(ctx, tradeLockKey, lockValue, lockTimeout).Result()
	if err != nil {
		log.Printf("Erro ao tentar adquirir lock de troca: %v", err)
		s.sendWebSocketMessage(player, "Erro interno no sistema de trocas. Tente novamente.")
		player.Deck = append(player.Deck, cardToTrade) // Devolve a carta
		return
	}

	if !ok {
		s.sendWebSocketMessage(player, "O sistema de trocas está ocupado. Tente novamente em alguns segundos.")
		player.Deck = append(player.Deck, cardToTrade) // Devolve a carta
		return
	}

	// Garante a liberação do lock
	defer func(val string) {
		script := redis.NewScript(`
			if redis.call("get", KEYS[1]) == ARGV[1] then
				return redis.call("del", KEYS[1])
			else
				return 0
			end
		`)
		script.Run(context.Background(), s.RedisClient, []string{tradeLockKey}, val)
	}(lockValue)

	// 2. Tenta pegar um ticket da fila (LPOP)
	ticketJSONReceived, err := s.RedisClient.LPop(ctx, tradeQueueKey).Result()

	// Cria o ticket do jogador ATUAL (ex: Jogador B)
	ticketToSend := TradeTicket{
		PlayerName: player.Name,
		ServerID:   s.ServerID,
		Card:       cardToTrade,
	}

	if err == redis.Nil {
		// CASO 1: FILA VAZIA (JOGADOR A)
		// Serializa e adiciona o ticket do jogador A à fila (RPUSH)
		ticketJSONToSend, _ := json.Marshal(ticketToSend)
		s.RedisClient.RPush(ctx, tradeQueueKey, ticketJSONToSend)

		log.Printf("Fila de trocas vazia. %s adicionou %s.", player.Name, cardToTrade.Name)
		s.sendWebSocketMessage(player, fmt.Sprintf("Sua carta '%s' foi adicionada à fila de trocas. Aguardando outro jogador...", cardToTrade.Name))
		return
	}

	if err != nil {
		// Erro real do Redis
		log.Printf("Erro ao dar LPOP na fila de trocas: %v", err)
		s.sendWebSocketMessage(player, "Erro interno ao acessar a fila de trocas. Tente novamente.")
		player.Deck = append(player.Deck, cardToTrade) // Devolve a carta
		return
	}

	// CASO 2: SUCESSO! (JOGADOR B)
	// Um ticket (do Jogador A) foi recebido.

	// Desserializa o ticket recebido (do Jogador A)
	var receivedTicket TradeTicket
	if err := json.Unmarshal([]byte(ticketJSONReceived), &receivedTicket); err != nil {
		log.Printf("Erro crítico ao desserializar ticket da fila de trocas: %v", err)
		s.sendWebSocketMessage(player, "Erro! O ticket na fila estava corrompido. Sua carta foi devolvida.")
		player.Deck = append(player.Deck, cardToTrade) // Devolve a carta B

		// Devolve o ticket corrompido à fila para não perdê-lo
		s.RedisClient.LPush(ctx, tradeQueueKey, ticketJSONReceived)
		return
	}

	receivedCard := receivedTicket.Card             // Carta do Jogador A
	receivedPlayerName := receivedTicket.PlayerName // Nome do Jogador A

	// 4. Adiciona a carta recebida (de A) ao deck do Jogador B (local)
	player.Deck = append(player.Deck, receivedCard)

	log.Printf("Troca local bem-sucedida para %s. Enviou %s, Recebeu %s.", player.Name, cardToTrade.Name, receivedCard.Name)
	s.sendWebSocketMessage(player, fmt.Sprintf("Troca realizada! Você enviou '%s (Força: %d)' e recebeu '%s (Força: %d)'.", cardToTrade.Name, cardToTrade.Forca, receivedCard.Name, receivedCard.Forca))

	// --- 5. Notificar Jogador A via Pub/Sub ---

	// Prepara a mensagem para o Jogador A
	// Envia a carta do Jogador B, 'cardToTrade', para o Jogador A
	cardB_JSON, _ := json.Marshal(cardToTrade)
	messageForA := fmt.Sprintf("TRADE_COMPLETE|%s", string(cardB_JSON))
	channelForA := fmt.Sprintf("player:%s", receivedPlayerName)

	// Publica a mensagem
	if err := s.RedisClient.Publish(ctx, channelForA, messageForA).Err(); err != nil {
		log.Printf("FALHA CRÍTICA AO PUBLICAR TROCA para %s: %v", receivedPlayerName, err)
		// Lógica de compensação (ex: devolver a carta de A para a fila)
	} else {
		log.Printf("Notificação de troca enviada para %s (%s) via Pub/Sub.", receivedPlayerName, receivedCard.Name)
	}
}
