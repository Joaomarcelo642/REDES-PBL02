package main

import (
	"context"
	"encoding/json" // Importa json
	"fmt"
	"log"
	"math/rand"
	"strconv" // Importa strings
	"time"

	"github.com/gorilla/websocket"
)

// --- FUNÇÃO REESCRITA ---
// handleGameMove agora apenas escreve a jogada no Redis e publica um evento.
// Não chama mais determineWinner.
func (s *Server) handleGameMove(player *PlayerState, session *GameSession, command string) {
	// 1. Valida o comando e seleciona a carta
	choice, err := strconv.Atoi(command)
	if err != nil || (choice != 1 && choice != 2) {
		s.sendWebSocketMessage(player, "Comando inválido. Jogue '1' ou '2'.")
		return
	}

	// 2. Identifica o jogador e o ID do jogo
	// (Graças ao startLocalGame, Player1.Name sempre existe)
	session.mu.Lock()
	gameID := session.Player1.Name
	isP1 := (player.Name == session.Player1.Name)
	session.mu.Unlock()

	gameKey := fmt.Sprintf("game:state:%s", gameID)
	var field string
	var chosenCard Card

	// 3. Define a carta jogada e o campo do Redis
	if isP1 {
		field = "p1_card"
		chosenCard = session.Player1Hand[choice-1]
	} else {
		field = "p2_card"
		chosenCard = session.Player2Hand[choice-1]
	}

	ctx := context.Background()

	// 4. Verifica se a jogada já foi feita (no Redis)
	exists, err := s.RedisClient.HExists(ctx, gameKey, field).Result()
	if err != nil {
		log.Printf("Erro ao verificar HExists no Redis: %v", err)
		return
	}
	if exists {
		s.sendWebSocketMessage(player, "Você já fez sua jogada.")
		return
	}

	// 5. Salva a jogada no Redis
	cardJSON, err := json.Marshal(chosenCard)
	if err != nil {
		log.Printf("Erro ao serializar carta: %v", err)
		return
	}
	s.RedisClient.HSet(ctx, gameKey, field, cardJSON)

	// 6. Notifica o "cérebro" (o listener do P1-Server) que uma jogada foi feita
	gameChannel := fmt.Sprintf("game:channel:%s", gameID)
	s.RedisClient.Publish(ctx, gameChannel, "MOVE_MADE")

	log.Printf("Jogador %s jogou %s. (Escrito no Redis)", player.Name, chosenCard.Name)
}

// --- NOVA FUNÇÃO "CÉREBRO" ---
// listenForGameEvents é o "cérebro" da partida. Roda apenas no P1-Server.
// Escuta eventos de jogada (via Pub/Sub) e o timeout.
func (s *Server) listenForGameEvents(session *GameSession, gameID string) {
	ctx := context.Background()
	gameChannel := fmt.Sprintf("game:channel:%s", gameID)
	gameKey := fmt.Sprintf("game:state:%s", gameID)

	// 1. Subscribe to move notifications
	pubsub := s.RedisClient.Subscribe(ctx, gameChannel)
	defer pubsub.Close()

	ch := pubsub.Channel()

	// 2. Create the game turn timeout
	timeout := time.NewTimer(gameTurnTimeout)
	defer timeout.Stop()

	log.Printf("[Game %s]: Listener (P1-Server) aguardando jogadas ou timeout.", gameID)

	for {
		select {
		case msg := <-ch:
			// 3. Uma jogada foi feita (via handleGameMove)
			log.Printf("[Game %s]: Notificação recebida: %s", gameID, msg.Payload)

			// Verifica no Redis se AMBAS as jogadas estão lá
			moves, err := s.RedisClient.HGetAll(ctx, gameKey).Result()
			if err != nil {
				log.Printf("[Game %s]: Erro ao ler hash do Redis %s: %v", gameKey, err)
				continue
			}

			if p1CardJSON, ok1 := moves["p1_card"]; ok1 {
				if p2CardJSON, ok2 := moves["p2_card"]; ok2 {
					// AMBOS JOGARAM
					log.Printf("[Game %s]: Ambas as jogadas recebidas. Determinando vencedor.", gameID)
					s.fillSessionFromRedis(session, p1CardJSON, p2CardJSON)
					s.determineWinner(session)
					s.RedisClient.Del(ctx, gameKey) // Limpa o estado do jogo
					return                          // Encerra a goroutine
				}
			}
			// Se só um jogou, continua esperando...

		case <-timeout.C:
			// 4. TEMPO ESGOTADO
			log.Printf("[Game %s]: Timeout! Verificando jogadas e determinando vencedor.", gameID)

			// Pega o que tiver no Redis
			moves, _ := s.RedisClient.HGetAll(ctx, gameKey).Result()
			p1CardJSON, _ := moves["p1_card"]
			p2CardJSON, _ := moves["p2_card"]

			s.fillSessionFromRedis(session, p1CardJSON, p2CardJSON)
			s.determineWinner(session)
			s.RedisClient.Del(ctx, gameKey) // Limpa o estado do jogo
			return                          // Encerra a goroutine
		}
	}
}

// --- NOVA FUNÇÃO AUXILIAR ---
// fillSessionFromRedis preenche a sessão local (no P1-Server) com
// as cartas lidas do Redis antes de chamar determineWinner.
func (s *Server) fillSessionFromRedis(session *GameSession, p1CardJSON, p2CardJSON string) {
	session.mu.Lock()
	defer session.mu.Unlock()

	if p1CardJSON != "" {
		var card Card
		if json.Unmarshal([]byte(p1CardJSON), &card) == nil {
			session.Player1Card = &card
		}
	}
	if p2CardJSON != "" {
		var card Card
		if json.Unmarshal([]byte(p2CardJSON), &card) == nil {
			session.Player2Card = &card
		}
	}
}

// --- FUNÇÃO MODIFICADA ---
// determineWinner agora é chamado APENAS pelo P1-Server.
// Ela envia o resultado do P1 localmente e do P2 via Redis Pub/Sub.
func (s *Server) determineWinner(session *GameSession) {
	session.mu.Lock()
	defer session.mu.Unlock()

	// Prevenção contra chamada dupla
	if session.Player1.State != "InGame" {
		log.Printf("[Game %s]: determineWinner chamado, mas P1 não está InGame (provavelmente já terminou).", session.Player1.Name)
		return
	}

	p1Card := session.Player1Card
	p2Card := session.Player2Card
	var resultP1, resultP2, logMessage string

	// (Lógica original de comparação de cartas)
	if p1Card != nil && p2Card != nil {
		if p1Card.Forca > p2Card.Forca {
			resultP1 = fmt.Sprintf("RESULT|VITÓRIA|Sua carta %s (%d) venceu %s (%d) de %s.\n", p1Card.Name, p1Card.Forca, p2Card.Name, p2Card.Forca, session.Player2.Name)
			resultP2 = fmt.Sprintf("RESULT|DERROTA|Sua carta %s (%d) perdeu para %s (%d) de %s.\n", p2Card.Name, p2Card.Forca, p1Card.Name, p1Card.Forca, session.Player1.Name)
			logMessage = fmt.Sprintf("Resultado: %s venceu %s.", session.Player1.Name, session.Player2.Name)
		} else if p2Card.Forca > p1Card.Forca {
			resultP2 = fmt.Sprintf("RESULT|VITÓRIA|Sua carta %s (%d) venceu %s (%d) de %s.\n", p2Card.Name, p2Card.Forca, p1Card.Name, p1Card.Forca, session.Player1.Name)
			resultP1 = fmt.Sprintf("RESULT|DERROTA|Sua carta %s (%d) perdeu para %s (%d) de %s.\n", p1Card.Name, p1Card.Forca, p2Card.Name, p2Card.Forca, session.Player2.Name)
			logMessage = fmt.Sprintf("Resultado: %s venceu %s.", session.Player2.Name, session.Player1.Name)
		} else {
			result := fmt.Sprintf("RESULT|EMPATE|Empate! Ambas as cartas têm força %d.\n", p1Card.Forca)
			resultP1, resultP2 = result, result
			logMessage = fmt.Sprintf("Resultado: Empate entre %s e %s.", session.Player1.Name, session.Player2.Name)
		}
	} else if p1Card == nil && p2Card != nil {
		resultP1 = "RESULT|DERROTA|Você não jogou a tempo e perdeu.\n"
		resultP2 = fmt.Sprintf("RESULT|VITÓRIA|%s não jogou a tempo. Você venceu!\n", session.Player1.Name)
		logMessage = fmt.Sprintf("Resultado: %s venceu %s por timeout.", session.Player2.Name, session.Player1.Name)
	} else if p2Card == nil && p1Card != nil {
		resultP2 = "RESULT|DERROTA|Você não jogou a tempo e perdeu.\n"
		resultP1 = fmt.Sprintf("RESULT|VITÓRIA|%s não jogou a tempo. Você venceu!\n", session.Player2.Name)
		logMessage = fmt.Sprintf("Resultado: %s venceu %s por timeout.", session.Player1.Name, session.Player2.Name)
	} else {
		result := "RESULT|EMPATE|Nenhum jogador jogou a tempo. Empate.\n"
		resultP1, resultP2 = result, result
		logMessage = fmt.Sprintf("Resultado: Empate por timeout duplo entre %s e %s.", session.Player1.Name, session.Player2.Name)
	}

	log.Printf("Partida entre %s e %s finalizada. %s", session.Player1.Name, session.Player2.Name, logMessage)

	// --- LÓGICA DE ENVIO MODIFICADA ---
	// Envia para P1 (jogador local) via WebSocket
	if session.Player1 != nil && session.Player1.WsConn != nil {
		if resultP1 != "" {
			if err := session.Player1.WsConn.WriteMessage(websocket.TextMessage, []byte(resultP1)); err != nil {
				log.Printf("Erro ao enviar resultado para %s: %v", session.Player1.Name, err)
			}
		}
	}

	// Envia para P2 (jogador remoto) via Redis Pub/Sub
	if session.Player2 != nil && resultP2 != "" {
		p2Channel := fmt.Sprintf("player:%s", session.Player2.Name)
		if err := s.RedisClient.Publish(context.Background(), p2Channel, resultP2).Err(); err != nil {
			log.Printf("Erro ao publicar resultado para %s via Redis: %v", session.Player2.Name, err)
		}
	}

	// --- LIMPEZA DE ESTADO ---
	// Reseta o estado do P1 (local)
	if session.Player1 != nil {
		session.Player1.mu.Lock()
		session.Player1.State = "Menu"
		session.Player1.CurrentGame = nil
		session.Player1.mu.Unlock()
	}
	// (O estado do P2 será limpo pelo listenRedisPubSub no P2-Server)

	// Remove a sessão do mapa de jogos ativos (APENAS no P1-Server)
	s.GamesMutex.Lock()
	if session.Player1 != nil {
		delete(s.ActiveGames, session.Player1.Name)
	}
	s.GamesMutex.Unlock()
}

// selectRandomCards (Função inalterada)
func selectRandomCards(deck []Card, count int) []Card {
	if len(deck) < count {
		return nil
	}
	rand.Seed(time.Now().UnixNano())

	// --- CORREÇÃO AQUI ---
	// Deve ser make([]Card, len(deck)) e não []byte
	deckCopy := make([]Card, len(deck))
	copy(deckCopy, deck) // Agora os tipos são compatíveis ([]Card, []Card)
	// --- FIM DA CORREÇÃO ---

	rand.Shuffle(len(deckCopy), func(i, j int) {
		deckCopy[i], deckCopy[j] = deckCopy[j], deckCopy[i]
	})

	// Agora o tipo de retorno é o correto ([]Card)
	return deckCopy[:count]
}
