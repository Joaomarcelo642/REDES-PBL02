package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
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

	// --- ATUALIZA ESTADO DO JOGADOR ---
	player.mu.Lock()
	player.State = "Searching"
	player.mu.Unlock()

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
		player.mu.Lock()
		player.State = "Menu" // Reverte o estado
		player.mu.Unlock()
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

	// Cria um ticket JSON apenas para a remoção (ZRem)
	// Nota: Isso é frágil. Se o timestamp for diferente, não removerá.
	// Uma abordagem melhor seria ZRemRangeByScore, mas vamos manter simples.
	player.mu.Lock()
	// Se o jogador não estiver mais "Searching", ele já foi pareado.
	if player.State != "Searching" {
		player.mu.Unlock()
		return
	}
	// Se ainda estiver "Searching", reverte para "Menu"
	player.State = "Menu"
	player.mu.Unlock()

	// Tenta remover o jogador da fila.
	// Precisamos iterar para encontrar o ticket certo, pois não temos o timestamp exato.
	// [Simplificação: Vamos assumir que ZRem por um JSON parcial funciona - o que não funciona]
	// [Correção de Lógica]: O ZRem original estava errado.
	// Vamos buscar na fila por PlayerName.
	members, err := s.RedisClient.ZRange(ctx, matchmakingQueueKey, 0, -1).Result()
	if err != nil {
		log.Printf("Erro ao ler fila para timeout de %s: %v", player.Name, err)
		return
	}

	var ticketToRemove string
	for _, member := range members {
		if strings.Contains(member, fmt.Sprintf(`"player_name":"%s"`, player.Name)) {
			ticketToRemove = member
			break
		}
	}

	if ticketToRemove != "" {
		removed, _ := s.RedisClient.ZRem(ctx, matchmakingQueueKey, ticketToRemove).Result()
		if removed > 0 {
			// Se foi removido, significa que o timeout ocorreu e ele não foi pareado.
			s.sendWebSocketMessage(player, "NO_MATCH_FOUND")
			log.Printf("Jogador %s removido da fila por timeout.", player.Name)
		}
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
		defer func(val string) {
			script := redis.NewScript(`
				if redis.call("get", KEYS[1]) == ARGV[1] then
					return redis.call("del", KEYS[1])
				else
					return 0
				end
			`)
			// Usamos um contexto novo para o defer, caso o principal expire
			script.Run(context.Background(), s.RedisClient, []string{matchmakingLockKey}, val)
		}(lockValue) // Passa o lockValue para o defer

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
// --- ESTA FUNÇÃO FOI MODIFICADA (TOLERÂNCIA A FALHAS) ---
func (s *Server) notifyMatchStart(p1Ticket, p2Ticket MatchmakingTicket) {
	log.Printf("Iniciando notificação de partida para %s vs %s", p1Ticket.PlayerName, p2Ticket.PlayerName)

	// 1. Caso Local: Ambos os jogadores no mesmo servidor (o que encontrou a partida)
	if p1Ticket.ServerID == s.ServerID && p2Ticket.ServerID == s.ServerID {
		s.startLocalGame(p1Ticket.PlayerName, p2Ticket.PlayerName)
		s.startLocalGame(p2Ticket.PlayerName, p1Ticket.PlayerName)
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

	// Notifica o servidor do Jogador 1 (se for remoto)
	if p1Ticket.ServerID != s.ServerID {
		err := s.callRemoteMatchNotification(p1Ticket.ServerID, req)
		if err != nil {
			// --- CORREÇÃO DE FALHA ---
			// Se a notificação falhar, aborta a partida.
			log.Printf("FALHA AO NOTIFICAR P1 (%s) no servidor %s. Partida abortada. Erro: %v", p1Ticket.PlayerName, p1Ticket.ServerID, err)
			// TODO: Idealmente, deveria devolver os jogadores à fila.
			// Por enquanto, apenas abortamos o início do jogo.
			return
		}
	}

	// Notifica o servidor do Jogador 2 (se for remoto)
	if p2Ticket.ServerID != s.ServerID {
		err := s.callRemoteMatchNotification(p2Ticket.ServerID, req)
		if err != nil {
			// --- CORREÇÃO DE FALHA ---
			log.Printf("FALHA AO NOTIFICAR P2 (%s) no servidor %s. Partida abortada. Erro: %v", p2Ticket.PlayerName, p2Ticket.ServerID, err)
			// TODO: Idealmente, deveria devolver P1 (se notificado) e P2 à fila.
			return
		}
	}

	// SOMENTE SE AS NOTIFICAÇÕES REMOTAS FOREM BEM SUCEDIDAS,
	// iniciamos o jogo para os jogadores locais.

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
// --- ESTA FUNÇÃO FOI MODIFICADA (URL E RETORNO DE ERRO) ---
func (s *Server) callRemoteMatchNotification(remoteServerID string, req MatchNotificationRequest) error {
	// O endereço do servidor remoto é resolvido pelo nome do serviço Docker (server-X)
	// ...
	// Por simplicidade, assumimos que o nome do serviço é o ServerID (ex: server-1)

	// --- CORREÇÃO 1: Mudar de %s:%s para %s%s
	// Isso combina "server-1" (remoteServerID) com ":8081" (restPort)
	// para formar "http://server-1:8081/..."
	url := fmt.Sprintf("http://%s%s/api/v1/match/notify", remoteServerID, restPort)

	jsonData, _ := json.Marshal(req)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Erro ao notificar servidor %s via REST: %v", remoteServerID, err)
		return err // --- CORREÇÃO 2: Retorna o erro de HTTP
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Servidor %s retornou status %d ao notificar partida.", remoteServerID, resp.StatusCode)
		return fmt.Errorf("servidor remoto retornou status %d", resp.StatusCode) // --- CORREÇÃO 3: Retorna o erro de status
	}

	return nil // Sucesso
}

// startLocalGame inicia a sessão de jogo entre dois jogadores (um pode ser remoto).
func (s *Server) startLocalGame(localPlayerName, opponentPlayerName string) {
	// 1. Pega o jogador local do mapa
	s.PlayerMutex.Lock()
	localPlayer, ok := s.Players[localPlayerName]
	s.PlayerMutex.Unlock()

	if !ok {
		log.Printf("Erro: startLocalGame chamado para jogador local %s, mas não encontrado.", localPlayerName)
		return
	}

	// 2. Protege o mapa de jogos ativos
	s.GamesMutex.Lock()
	defer s.GamesMutex.Unlock()

	var session *GameSession
	var hand [2]Card
	var handStr string

	// 3. Tenta encontrar uma sessão existente (criada pelo oponente)
	// (No teste local-vs-local, o oponente também está no 's.ActiveGames')
	session, exists := s.ActiveGames[opponentPlayerName]

	if !exists {
		// 4. Se não existe, este é o Jogador 1. Cria a sessão.
		log.Printf("Iniciando partida (P1): %s vs %s.", localPlayerName, opponentPlayerName)

		// Pega a mão do P1
		handCards := selectRandomCards(localPlayer.Deck, 2)
		if handCards == nil {
			log.Printf("Erro: %s não tem cartas suficientes para jogar.", localPlayerName)
			s.sendWebSocketMessage(localPlayer, "Erro: Você não tem cartas suficientes (mínimo 2).")
			return
		}
		hand[0] = handCards[0]
		hand[1] = handCards[1]

		// Cria a sessão
		session = &GameSession{
			Player1:     localPlayer,
			Player1Hand: hand,
			mu:          sync.Mutex{},
		}

		// Adiciona ao mapa de jogos
		s.ActiveGames[localPlayerName] = session

		handStr = fmt.Sprintf("MATCH_START|%s (%d)|%s (%d)", hand[0].Name, hand[0].Forca, hand[1].Name, hand[1].Forca)

	} else {
		// 5. Se existe, este é o Jogador 2. Entra na sessão.
		log.Printf("Iniciando partida (P2): %s vs %s.", localPlayerName, opponentPlayerName)

		// Pega a mão do P2
		handCards := selectRandomCards(localPlayer.Deck, 2)
		if handCards == nil {
			log.Printf("Erro: %s não tem cartas suficientes para jogar.", localPlayerName)
			s.sendWebSocketMessage(localPlayer, "Erro: Você não tem cartas suficientes (mínimo 2).")
			return
		}
		hand[0] = handCards[0]
		hand[1] = handCards[1]

		// Adiciona P2 à sessão
		session.mu.Lock()
		session.Player2 = localPlayer
		session.Player2Hand = hand
		session.mu.Unlock()

		// Move a sessão no mapa para o nome do P1 (chave principal)
		// (Nota: No caso local-local, `startLocalGame` é chamado para P1 e P2.
		// P1 cria (ActiveGames[P1]), P2 encontra (ActiveGames[P1]) e se adiciona.)
		// Precisamos garantir que a chave seja consistente.
		// A lógica atual (P1 cria, P2 encontra) funciona.
		// O `notifyMatchStart` garante que `startLocalGame` seja chamado para P1 e P2.

		handStr = fmt.Sprintf("MATCH_START|%s (%d)|%s (%d)", hand[0].Name, hand[0].Forca, hand[1].Name, hand[1].Forca)
	}

	// 6. Atualiza o estado do jogador local
	localPlayer.mu.Lock()
	localPlayer.State = "InGame"
	localPlayer.CurrentGame = session
	localPlayer.mu.Unlock()

	// 7. Envia mensagens de início
	s.sendWebSocketMessage(localPlayer, "MATCH_FOUND")
	s.sendWebSocketMessage(localPlayer, handStr)

	// Inicia o timer de jogada
	timerMsg := fmt.Sprintf("TIMER|%d", int(gameTurnTimeout.Seconds()))
	s.sendWebSocketMessage(localPlayer, timerMsg)
}
