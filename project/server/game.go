package main

import (
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/gorilla/websocket"
)

// selectRandomCards pega uma cópia do deck do jogador, embaralha e retorna o número de cartas solicitado.
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
func determineWinner(session *GameSession) {
	session.mu.Lock()
	defer session.mu.Unlock()

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
}
