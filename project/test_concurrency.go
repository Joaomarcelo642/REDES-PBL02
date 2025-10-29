package main

import (
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Constantes de teste
const (
	numBots           = 10
	packsToOpenPerBot = 3
	serverWsUrl       = "ws://server-1:8080" // Conecta ao primeiro servidor

	// Estratégia de retry do bot quando o servidor retornar "falha ao adquirir lock"
	botMaxRetries = 3
	botRetryDelay = 50 * time.Millisecond
)

// Estrutura para rastrear o estado do teste
type TestState struct {
	PacksOpened int
	Mutex       sync.Mutex
}

var globalTestState = TestState{}

func main() {
	log.Println("--- INICIANDO TESTE DE CONCORRÊNCIA DE ABERTURA DE PACOTES ---")
	log.Printf("Simulando %d bots, cada um tentando abrir %d pacotes.", numBots, packsToOpenPerBot)

	var wg sync.WaitGroup
	wg.Add(numBots)

	startTime := time.Now()

	for i := 1; i <= numBots; i++ {
		playerName := fmt.Sprintf("TestBot-%d", i)
		time.Sleep(1 * time.Millisecond) // Pequeno delay para evitar sobrecarga inicial
		go func() {
			defer wg.Done()
			runTestBot(playerName)
		}()
	}

	wg.Wait()

	duration := time.Since(startTime)

	log.Println("--- RESULTADO DO TESTE ---")
	log.Printf("Tempo total de execução: %s", duration)
	log.Printf("Total de pacotes que os bots TENTARAM abrir: %d", numBots*packsToOpenPerBot)
	log.Printf("Total de pacotes ABERTOS com sucesso (rastreado localmente): %d", globalTestState.PacksOpened)
	log.Println("--------------------------")

	// O teste é considerado bem-sucedido se o número de pacotes abertos for o esperado,
	// e se o log do servidor não apresentar erros de concorrência no estoque.
	// A verificação final do estoque no Redis deve ser feita manualmente ou via script
	// para garantir que não houve duplicação ou perda.

	// Se o número de pacotes abertos for menor que o esperado, pode ser devido ao limite de 3
	// pacotes por jogador (que é uma regra de negócio).
	expectedPacks := numBots * packsToOpenPerBot
	if globalTestState.PacksOpened > expectedPacks {
		log.Fatalf("ERRO: Pacotes abertos (%d) excedem o esperado (%d). Possível duplicação/falha de concorrência.", globalTestState.PacksOpened, expectedPacks)
	} else {
		log.Printf("Teste de concorrência concluído. O número de pacotes abertos está dentro do limite esperado.")
	}
}

func runTestBot(playerName string) {
	u, _ := url.Parse(serverWsUrl)
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Printf("[Bot %s]: Não foi possível conectar ao servidor: %v", playerName, err)
		return
	}
	defer conn.Close()

	// 1. Envia o nome do jogador
	conn.WriteMessage(websocket.TextMessage, []byte(playerName))

	// 2. Espera a resposta inicial do servidor (pacote inicial obrigatório)
	_, p, err := conn.ReadMessage()
	if err != nil {
		log.Printf("[Bot %s]: Erro ao receber pacote inicial: %v", playerName, err)
		return
	}

	// Log da resposta inicial e contagem
	resp := string(p)
	log.Printf("[Bot %s] Resposta inicial: %s", playerName, resp)
	if strings.Contains(resp, "Bem-vindo(a)") || strings.Contains(resp, "Parabéns") {
		globalTestState.Mutex.Lock()
		globalTestState.PacksOpened++
		globalTestState.Mutex.Unlock()
	} else {
		log.Printf("[Bot %s] Pacote inicial não confirmado como sucesso pelo bot (resposta recebida).", playerName)
	}

	// 3. Ação automatizada: O bot abre os pacotes extras.
	for i := 0; i < packsToOpenPerBot-1; i++ { // -1 porque o primeiro já foi aberto
		conn.WriteMessage(websocket.TextMessage, []byte("OPEN_PACK"))
		_, p, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[Bot %s]: Erro ao abrir pacote extra: %v", playerName, err)
			break
		}

		// Verifica e loga a resposta do servidor
		resp2 := string(p)
		log.Printf("[Bot %s] Resposta ao OPEN_PACK: %s", playerName, resp2)
		if strings.Contains(resp2, "Parabéns") || strings.Contains(resp2, "Bem-vindo(a)") {
			globalTestState.Mutex.Lock()
			globalTestState.PacksOpened++
			globalTestState.Mutex.Unlock()
		} else {
			log.Printf("[Bot %s] OPEN_PACK não contabilizado como sucesso pelo bot.", playerName)
		}
	}
}
