package main

import (
	"fmt"
	"log"
	"math/rand"
	"strconv" // Importa strconv
	"time"

	"github.com/gorilla/websocket"
)

// --- NOVA FUNÇÃO ---
// handleGameMove processa a jogada de um jogador ("1" ou "2")
func (s *Server) handleGameMove(player *PlayerState, session *GameSession, command string) {
	session.mu.Lock()
	defer session.mu.Unlock()

	// 1. Verifica se a jogada já foi feita
	isP1 := (player.Name == session.Player1.Name)
	if (isP1 && session.Player1Card != nil) || (!isP1 && session.Player2Card != nil) {
		s.sendWebSocketMessage(player, "Você já fez sua jogada.")
		return
	}

	// 2. Valida o comando e seleciona a carta
	choice, err := strconv.Atoi(command)
	if err != nil || (choice != 1 && choice != 2) {
		s.sendWebSocketMessage(player, "Comando inválido. Jogue '1' ou '2'.")
		return
	}

	// 3. Define a carta jogada na sessão
	if isP1 {
		chosenCard := session.Player1Hand[choice-1]
		session.Player1Card = &chosenCard
		log.Printf("Jogador %s (P1) jogou %s.", player.Name, chosenCard.Name)
	} else {
		// Garante que P2 exista antes de tentar acessá-lo
		if session.Player2 == nil || player.Name != session.Player2.Name {
			log.Printf("Erro: Jogador %s não é P2 nesta sessão.", player.Name)
			return
		}
		chosenCard := session.Player2Hand[choice-1]
		session.Player2Card = &chosenCard
		log.Printf("Jogador %s (P2) jogou %s.", player.Name, chosenCard.Name)
	}

	// 4. Se ambos jogaram, determina o vencedor
	if session.Player1Card != nil && session.Player2Card != nil {
		// Chama determineWinner em uma nova goroutine para liberar o lock da sessão
		// e permitir que determineWinner gerencie seus próprios locks (jogador, etc.)
		go s.determineWinner(session)
	}
}

// selectRandomCards (Função inalterada)
func selectRandomCards(deck []Card, count int) []Card {
	if len(deck) < count {
		return nil
	}
	rand.Seed(time.Now().UnixNano())
	deckCopy := make([]Card, len(deck))
	copy(deckCopy, deck)
	rand.Shuffle(len(deckCopy), func(i, j int) {
		deckCopy[i], deckCopy[j] = deckCopy[j], deckCopy[i]
	})
	return deckCopy[:count]
}

// determineWinner compara as cartas jogadas e envia o resultado para ambos os jogadores.
// --- ESTA FUNÇÃO FOI MODIFICADA ---
func (s *Server) determineWinner(session *GameSession) {
	session.mu.Lock()
	defer session.mu.Unlock()

	// Prevenção contra chamada dupla (embora handleGameMove deva prevenir)
	if session.Player1.State != "InGame" {
		return
	}

	p1Card := session.Player1Card
	p2Card := session.Player2Card
	var resultP1, resultP2, logMessage string

	// ... Lógica de determineWinner (mantida a original) ...
	if p1Card != nil && p2Card != nil {
		// Caso 1: Ambos jogaram. Compara a força das cartas.
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
		// Caso 2: Apenas o jogador 2 jogou.
		resultP1 = "RESULT|DERROTA|Você não jogou a tempo e perdeu.\n"
		resultP2 = fmt.Sprintf("RESULT|VITÓRIA|%s não jogou a tempo. Você venceu!\n", session.Player1.Name)
		logMessage = fmt.Sprintf("Resultado: %s venceu %s por timeout.", session.Player2.Name, session.Player1.Name)
	} else if p2Card == nil && p1Card != nil {
		// Caso 3: Apenas o jogador 1 jogou.
		resultP2 = "RESULT|DERROTA|Você não jogou a tempo e perdeu.\n"
		resultP1 = fmt.Sprintf("RESULT|VITÓRIA|%s não jogou a tempo. Você venceu!\n", session.Player2.Name)
		logMessage = fmt.Sprintf("Resultado: %s venceu %s por timeout.", session.Player1.Name, session.Player2.Name)
	} else {
		// Caso 4: Nenhum jogador jogou.
		result := "RESULT|EMPATE|Nenhum jogador jogou a tempo. Empate.\n"
		resultP1, resultP2 = result, result
		logMessage = fmt.Sprintf("Resultado: Empate por timeout duplo entre %s e %s.", session.Player1.Name, session.Player2.Name)
	}

	// Como o PlayerState agora usa WebSocket, enviamos as mensagens diretamente para as conexões.
	log.Printf("Partida entre %s e %s finalizada. %s", session.Player1.Name, session.Player2.Name, logMessage)

	if session.Player1 != nil && session.Player1.WsConn != nil {
		if resultP1 != "" {
			if err := session.Player1.WsConn.WriteMessage(websocket.TextMessage, []byte(resultP1)); err != nil {
				log.Printf("Erro ao enviar resultado para %s: %v", session.Player1.Name, err)
			}
		}
	}
	if session.Player2 != nil && session.Player2.WsConn != nil {
		if resultP2 != "" {
			if err := session.Player2.WsConn.WriteMessage(websocket.TextMessage, []byte(resultP2)); err != nil {
				log.Printf("Erro ao enviar resultado para %s: %v", session.Player2.Name, err)
			}
		}
	}

	// --- LIMPEZA DE ESTADO ---
	// Reseta o estado dos jogadores para "Menu"
	if session.Player1 != nil {
		session.Player1.mu.Lock()
		session.Player1.State = "Menu"
		session.Player1.CurrentGame = nil
		session.Player1.mu.Unlock()
	}
	if session.Player2 != nil {
		session.Player2.mu.Lock()
		session.Player2.State = "Menu"
		session.Player2.CurrentGame = nil
		session.Player2.mu.Unlock()
	}

	// Remove a sessão do mapa de jogos ativos
	s.GamesMutex.Lock()
	if session.Player1 != nil {
		delete(s.ActiveGames, session.Player1.Name)
	} else if session.Player2 != nil {
		// Caso P1 tenha desconectado, P2 pode ser a chave (embora a lógica atual use P1)
		delete(s.ActiveGames, session.Player2.Name)
	}
	s.GamesMutex.Unlock()
}