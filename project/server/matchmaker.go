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
	// NOTA: A função startLocalGame (no final) precisa destes imports,
	// certifique-se que eles estejam aqui.
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
func (s *Server) distributedMatchmaker() {
	ctx := context.Background()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Tenta adquirir um lock distribuído
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
			script.Run(context.Background(), s.RedisClient, []string{matchmakingLockKey}, val)
		}(lockValue)

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

		// Remove os jogadores da fila atomicamente
		removed, err := s.RedisClient.ZRem(ctx, matchmakingQueueKey, p1TicketJson, p2TicketJson).Result()
		if err != nil || removed != 2 {
			// Se não removeu 2, significa que outro servidor já os removeu
			continue
		}

		log.Printf("Pareamento confirmado: %s (Srv: %s) vs %s (Srv: %s)",
			p1Ticket.PlayerName, p1Ticket.ServerID, p2Ticket.PlayerName, p2Ticket.ServerID)

		// Notifica os servidores envolvidos para iniciar a partida
		s.notifyMatchStart(p1Ticket, p2Ticket)
	}
}

// notifyMatchStart coordena o início da partida entre os servidores.
// --- ESTA FUNÇÃO FOI CORRIGIDA ---
func (s *Server) notifyMatchStart(p1Ticket, p2Ticket MatchmakingTicket) {
	log.Printf("Iniciando notificação de partida para %s vs %s", p1Ticket.PlayerName, p2Ticket.PlayerName)

	req := MatchNotificationRequest{
		Player1Name: p1Ticket.PlayerName,
		Player2Name: p2Ticket.PlayerName,
		Server1ID:   p1Ticket.ServerID,
		Server2ID:   p2Ticket.ServerID,
	}

	// 1. Notifica o servidor do Jogador 1 (se for remoto)
	if p1Ticket.ServerID != s.ServerID {
		err := s.callRemoteMatchNotification(p1Ticket.ServerID, req)
		if err != nil {
			log.Printf("FALHA AO NOTIFICAR P1 (%s) no servidor %s. Partida abortada. Erro: %v", p1Ticket.PlayerName, p1Ticket.ServerID, err)
			// TODO: Devolver jogadores à fila
			return
		}
	}

	// 2. Notifica o servidor do Jogador 2 (se for remoto)
	if p2Ticket.ServerID != s.ServerID {
		err := s.callRemoteMatchNotification(p2Ticket.ServerID, req)
		if err != nil {
			log.Printf("FALHA AO NOTIFICAR P2 (%s) no servidor %s. Partida abortada. Erro: %v", p2Ticket.PlayerName, p2Ticket.ServerID, err)
			// TODO: Devolver jogadores à fila
			return
		}
	}

	// 3. Inicia o jogo para os jogadores que estão NESTE servidor (o orquestrador).
	// Ambas as chamadas usam a *ordem correta* (P1, P2) e passam *todos* os IDs.
	// A própria startLocalGame vai descobrir se o jogador local é P1 ou P2.

	if p1Ticket.ServerID == s.ServerID {
		s.startLocalGame(req.Player1Name, req.Player2Name, req.Server1ID, req.Server2ID)
	}

	if p2Ticket.ServerID == s.ServerID {
		// --- BUG CORRIGIDO ---
		// Chamamos com a ordem correta (P1, P2), não invertida.
		s.startLocalGame(req.Player1Name, req.Player2Name, req.Server1ID, req.Server2ID)
	}
}

// callRemoteMatchNotification envia a notificação de partida para um servidor remoto via REST.
func (s *Server) callRemoteMatchNotification(remoteServerID string, req MatchNotificationRequest) error {
	// O endereço do servidor remoto é resolvido pelo nome do serviço Docker
	url := fmt.Sprintf("http://%s%s/api/v1/match/notify", remoteServerID, restPort)

	jsonData, _ := json.Marshal(req)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Erro ao notificar servidor %s via REST: %v", remoteServerID, err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Servidor %s retornou status %d ao notificar partida.", remoteServerID, resp.StatusCode)
		return fmt.Errorf("servidor remoto retornou status %d", resp.StatusCode)
	}

	return nil // Sucesso
}

// startLocalGame (VERSÃO DISTRIBUÍDA)
// Esta é a função correta da sua mensagem anterior.
// Inicia a sessão de jogo. P1, P2 e seus IDs de servidor são fornecidos pelo matchmaker.
func (s *Server) startLocalGame(player1Name, player2Name, server1ID, server2ID string) {
	// 1. Pega o jogador local do mapa, identificando se é P1 ou P2
	s.PlayerMutex.Lock()
	var localPlayer *PlayerState
	var isP1 bool

	if p, ok := s.Players[player1Name]; ok {
		localPlayer = p
		isP1 = true
	} else if p, ok := s.Players[player2Name]; ok {
		localPlayer = p
		isP1 = false
	} else {
		s.PlayerMutex.Unlock()
		log.Printf("Erro: startLocalGame (P1: %s, P2: %s) chamado, mas nenhum jogador é local.", player1Name, player2Name)
		return
	}
	s.PlayerMutex.Unlock()

	// 2. Pega a mão do jogador local
	handCards := selectRandomCards(localPlayer.Deck, 2)
	if handCards == nil {
		log.Printf("Erro: %s não tem cartas suficientes para jogar.", localPlayer.Name)
		s.sendWebSocketMessage(localPlayer, "Erro: Você não tem cartas suficientes (mínimo 2).")
		return
	}
	var hand [2]Card
	hand[0] = handCards[0]
	hand[1] = handCards[1]

	// 3. Trava o mapa de jogos e cria/atualiza a sessão
	// A chave da sessão é SEMPRE player1Name.
	s.GamesMutex.Lock()

	session, exists := s.ActiveGames[player1Name]
	if !exists {
		session = &GameSession{
			mu: sync.Mutex{},
		}
		s.ActiveGames[player1Name] = session
	}

	// 4. Preenche os dados da sessão (local + "fantasma" remoto)
	session.mu.Lock()
	session.Server1ID = server1ID
	session.Server2ID = server2ID

	if isP1 {
		log.Printf("Iniciando partida (P1): %s vs %s.", player1Name, player2Name)
		session.Player1 = localPlayer
		session.Player1Hand = hand
		// Cria um "fantasma" para o P2
		session.Player2 = &PlayerState{Name: player2Name, ServerID: server2ID}
	} else {
		// O jogador local é P2
		log.Printf("Iniciando partida (P2): %s vs %s.", localPlayer.Name, player1Name)
		session.Player2 = localPlayer
		session.Player2Hand = hand
		// Cria um "fantasma" para o P1
		session.Player1 = &PlayerState{Name: player1Name, ServerID: server1ID}
	}
	session.mu.Unlock()
	s.GamesMutex.Unlock()

	// 5. Atualiza o estado do jogador local
	localPlayer.mu.Lock()
	localPlayer.State = "InGame"
	localPlayer.CurrentGame = session
	localPlayer.mu.Unlock()

	// 6. Envia mensagens de início
	s.sendWebSocketMessage(localPlayer, "MATCH_FOUND")
	handStr := fmt.Sprintf("MATCH_START|%s (%d)|%s (%d)", hand[0].Name, hand[0].Forca, hand[1].Name, hand[1].Forca)
	s.sendWebSocketMessage(localPlayer, handStr)
	timerMsg := fmt.Sprintf("TIMER|%d", int(gameTurnTimeout.Seconds()))
	s.sendWebSocketMessage(localPlayer, timerMsg)

	// 7. --- O CÉREBRO DO JOGO ---
	// Apenas o servidor do P1 (o "master") escuta os eventos e o timeout.
	if isP1 {
		log.Printf("Servidor P1 (%s) iniciando listener para jogo %s.", s.ServerID, player1Name)
		// s.listenForGameEvents é a função que você deve adicionar ao game.go
		go s.listenForGameEvents(session, player1Name)
	}
}
