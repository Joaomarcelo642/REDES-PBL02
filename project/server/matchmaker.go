package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	matchmakingQueueKey = "matchmaking_queue"
	matchmakingLockKey  = "lock:matchmaker"
)

// addToMatchmakingQueue adiciona o jogador à fila de matchmaking distribuída (Redis ZSET).
func (s *Server) addToMatchmakingQueue(player *PlayerState) {
	ctx := context.Background()

	// Cria o ticket de matchmaking
	ticket := MatchmakingTicket{
		PlayerName: player.Name,
		ServerID:   s.ServerID,
		Timestamp:  time.Now().Unix(),
	}
	ticketJson, _ := json.Marshal(ticket)

	// Adiciona o jogador à fila (ZSET) com o timestamp como score (para FIFO)
	_, err := s.RedisClient.ZAdd(ctx, matchmakingQueueKey, &redis.Z{
		Score:  float64(ticket.Timestamp),
		Member: string(ticketJson),
	}).Result()

	if err != nil {
		log.Printf("Erro ao adicionar %s à fila de matchmaking: %v", player.Name, err)
		s.sendWebSocketMessage(player, "Erro interno ao entrar na fila. Tente novamente.")
		return
	}

	s.sendWebSocketMessage(player, "Entrou na fila de matchmaking. Aguardando oponente...")

	// Inicia um timeout para o jogador
	go s.matchmakingTimeout(player, matchmakingTimeout)
}

// matchmakingTimeout remove o jogador da fila se o tempo esgotar.
func (s *Server) matchmakingTimeout(player *PlayerState, timeout time.Duration) {
	time.Sleep(timeout)

	ctx := context.Background()

	// Tenta remover o jogador da fila. Se ele já foi pareado, essa operação falhará silenciosamente.
	removed, err := s.RedisClient.ZRem(ctx, matchmakingQueueKey, fmt.Sprintf(`{"player_name":"%s"`, player.Name)).Result()
	if err != nil {
		log.Printf("Erro ao tentar remover %s da fila por timeout: %v", player.Name, err)
		return
	}

	if removed > 0 {
		// Se foi removido, significa que o timeout ocorreu e ele não foi pareado.
		s.sendWebSocketMessage(player, "NO_MATCH_FOUND")
		log.Printf("Jogador %s removido da fila por timeout.", player.Name)
	}
}

// distributedMatchmaker é a goroutine que roda em cada servidor para tentar parear jogadores.
// Item 6: Pareamento em Ambiente Distribuído
func (s *Server) distributedMatchmaker() {
	ctx := context.Background()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Tenta adquirir um lock distribuído para garantir que apenas um servidor
		// tente parear a cada momento, evitando pareamentos duplicados.
		lockValue := fmt.Sprintf("%s-%d", s.ServerID, time.Now().UnixNano())
		lockTimeout := 1 * time.Second

		ok, err := s.RedisClient.SetNX(ctx, matchmakingLockKey, lockValue, lockTimeout).Result()
		if err != nil {
			log.Printf("Erro ao tentar adquirir lock do matchmaker: %v", err)
			continue
		}

		if !ok {
			// Outro matchmaker está rodando.
			continue
		}

		// Garante a liberação do lock
		defer func() {
			script := redis.NewScript(`
				if redis.call("get", KEYS[1]) == ARGV[1] then
					return redis.call("del", KEYS[1])
				else
					return 0
				end
			`)
			script.Run(ctx, s.RedisClient, []string{matchmakingLockKey}, lockValue)
		}()

		// Tenta pegar os dois primeiros jogadores da fila
		members, err := s.RedisClient.ZRange(ctx, matchmakingQueueKey, 0, 1).Result()
		if err != nil {
			log.Printf("Erro ao ler fila de matchmaking: %v", err)
			continue
		}

		if len(members) < 2 {
			// Não há jogadores suficientes para parear
			continue
		}

		// Jogadores encontrados
		p1TicketJson := members[0]
		p2TicketJson := members[1]

		var p1Ticket, p2Ticket MatchmakingTicket
		if err := json.Unmarshal([]byte(p1TicketJson), &p1Ticket); err != nil {
			log.Printf("Erro ao desserializar ticket 1: %v", err)
			continue
		}
		if err := json.Unmarshal([]byte(p2TicketJson), &p2Ticket); err != nil {
			log.Printf("Erro ao desserializar ticket 2: %v", err)
			continue
		}

		// Remove os jogadores da fila atomicamente (garantindo o pareamento único)
		removed, err := s.RedisClient.ZRem(ctx, matchmakingQueueKey, p1TicketJson, p2TicketJson).Result()
		if err != nil || removed != 2 {
			// Se não removeu 2, significa que outro servidor já os removeu (ou um deles)
			continue
		}

		log.Printf("Pareamento confirmado: %s (Srv: %s) vs %s (Srv: %s)",
			p1Ticket.PlayerName, p1Ticket.ServerID, p2Ticket.PlayerName, p2Ticket.ServerID)

		// Notifica os servidores envolvidos para iniciar a partida
		s.notifyMatchStart(p1Ticket, p2Ticket)
	}
}

// notifyMatchStart coordena o início da partida entre os servidores.
func (s *Server) notifyMatchStart(p1Ticket, p2Ticket MatchmakingTicket) {
	log.Printf("Iniciando notificação de partida para %s vs %s", p1Ticket.PlayerName, p2Ticket.PlayerName)

	// 1. Caso Local: Ambos os jogadores no mesmo servidor (o que encontrou a partida)
	if p1Ticket.ServerID == s.ServerID && p2Ticket.ServerID == s.ServerID {
		s.startLocalGame(p1Ticket.PlayerName, p2Ticket.PlayerName)
		return
	}

	// 2. Caso Distribuído: Pelo menos um jogador está em outro servidor.
	// O servidor que encontrou a partida (s.ServerID) se torna o orquestrador.

	req := MatchNotificationRequest{
		Player1Name: p1Ticket.PlayerName,
		Player2Name: p2Ticket.PlayerName,
		Server1ID:   p1Ticket.ServerID,
		Server2ID:   p2Ticket.ServerID,
	}

	// Notifica o servidor do Jogador 1
	if p1Ticket.ServerID != s.ServerID {
		s.callRemoteMatchNotification(p1Ticket.ServerID, req)
	}

	// Notifica o servidor do Jogador 2
	if p2Ticket.ServerID != s.ServerID {
		s.callRemoteMatchNotification(p2Ticket.ServerID, req)
	}

	// Se o orquestrador tiver jogadores locais, inicia a partida para eles.
	if p1Ticket.ServerID == s.ServerID {
		s.startLocalGame(p1Ticket.PlayerName, p2Ticket.PlayerName)
	}
	if p2Ticket.ServerID == s.ServerID && p1Ticket.ServerID != s.ServerID {
		// Se P2 está local, mas P1 está remoto, o orquestrador inicia a partida local para P2.
		// A lógica de startLocalGame deve ser capaz de lidar com um jogador remoto.
		s.startLocalGame(p2Ticket.PlayerName, p1Ticket.PlayerName)
	}
}

// callRemoteMatchNotification envia a notificação de partida para um servidor remoto via REST.
func (s *Server) callRemoteMatchNotification(remoteServerID string, req MatchNotificationRequest) {
	// O endereço do servidor remoto é resolvido pelo nome do serviço Docker (server-X)
	// Como o barema pede emulação realista, o nome do serviço será "server-X"
	// No docker-compose, teremos múltiplos serviços "server" (server1, server2, etc.)
	// Por simplicidade, assumimos que o nome do serviço é o ServerID (ex: server-1)
	url := fmt.Sprintf("http://%s:%s/api/v1/match/notify", remoteServerID, restPort)

	jsonData, _ := json.Marshal(req)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Erro ao notificar servidor %s via REST: %v", remoteServerID, err)
		// TODO: Lógica de tolerância a falhas (Item 7) - o que fazer se o servidor falhar?
		// Por enquanto, apenas logamos o erro.
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Servidor %s retornou status %d ao notificar partida.", remoteServerID, resp.StatusCode)
	}
}

// startLocalGame inicia a sessão de jogo entre dois jogadores (um pode ser remoto).
func (s *Server) startLocalGame(localPlayerName, opponentPlayerName string) {
	// A lógica completa de handleGameSession será implementada na próxima fase,
	// mas aqui já preparamos a estrutura.

	localPlayer := s.Players[localPlayerName]
	
	// Envia a mensagem de partida encontrada para o jogador local.
	s.sendWebSocketMessage(localPlayer, "MATCH_FOUND")

	// TODO: Implementar a lógica de iniciar a sessão de jogo (handleGameSession)
	// Por enquanto, apenas um placeholder para simular o início.
	log.Printf("Iniciando partida local entre %s e %s.", localPlayerName, opponentPlayerName)
	
	// Simula o início da partida
	p1Card := Card{Name: "Carta Local 1", Forca: 5}
	p2Card := Card{Name: "Carta Local 2", Forca: 10}
	
	// Envia a mão para o jogador local
	p1HandStr := fmt.Sprintf("MATCH_START|%s (%d)|%s (%d)", p1Card.Name, p1Card.Forca, p2Card.Name, p2Card.Forca)
	s.sendWebSocketMessage(localPlayer, p1HandStr)

	// Inicia o timer de jogada (Item 5)
	timerMsg := fmt.Sprintf("TIMER|%d", int(gameTurnTimeout.Seconds()))
	s.sendWebSocketMessage(localPlayer, timerMsg)
}

