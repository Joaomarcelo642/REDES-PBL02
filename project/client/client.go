package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// O 'stateMutex' protege o acesso às variáveis de estado globais 'isSearching' e 'isInGame'.
var stateMutex sync.Mutex
var isSearching bool
var isInGame bool

// Tempo máximo, em segundos, que o cliente ficará na fila de matchmaking.
const matchmakingTimeoutSeconds = 15

// Função principal que inicializa e executa o cliente.
func main() {
	// Define e processa flags de linha de comando
	botMode := flag.Bool("bot", false, "Executa o cliente em modo automatizado (bot).")
	botCount := flag.Int("count", 1, "Número de bots a serem executados em paralelo.")
	botPrefix := flag.String("prefix", "Jogador", "Prefixo para o nome dos bots.")
	flag.Parse()

	// Pega os argumentos que não são flags, como o IP do servidor.
	args := flag.Args()
	if len(args) < 1 {
		log.Fatal("Uso: ./client [-bot] [-count N] [-prefix P] <ip_do_servidor> [nome_do_jogador_manual]")
	}
	serverIP := args[0]
	serverWsUrl := fmt.Sprintf("ws://%s:8080", serverIP)

	// Se o modo bot estiver ativado, o programa irá simular múltiplos jogadores.
	if *botMode {
		var wg sync.WaitGroup
		for i := 1; i <= *botCount; i++ {
			wg.Add(1)
			playerName := fmt.Sprintf("%s%d", *botPrefix, i)
			time.Sleep(10 * time.Millisecond)
			go func() {
				defer wg.Done()
				runBot(playerName, serverWsUrl)
			}()
		}
		wg.Wait()
		log.Printf("Todos os %d bots terminaram a execução.", *botCount)
	} else {
		// Modo interativo para um jogador humano.
		if len(args) < 2 {
			log.Fatal("Uso para modo interativo: ./client <ip_do_servidor> <nome_do_jogador>")
		}
		playerName := args[1]
		// O envio de pacotes UDP (keep-alive) foi removido, pois o WebSocket é persistente.
		// A funcionalidade de heartbeat deve ser tratada pelo protocolo WebSocket.
		handleServerConnection(playerName, serverWsUrl)
	}
}

// runBot define o comportamento de um cliente automatizado.
func runBot(playerName string, serverWsUrl string) {
	u, _ := url.Parse(serverWsUrl)
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Printf("[Bot %s]: Não foi possível conectar ao servidor: %v", playerName, err)
		return
	}
	defer conn.Close()

	// 1. Envia o nome do jogador
	err = conn.WriteMessage(websocket.TextMessage, []byte(playerName))
	if err != nil {
		log.Printf("[Bot %s]: Erro ao enviar nome: %v", playerName, err)
		return
	}

	// 2. Espera a resposta inicial do servidor para confirmar a conexão (pacote inicial)
	_, p, err := conn.ReadMessage()
	if err != nil {
		log.Printf("[Bot %s]: Erro ao receber pacote inicial: %v", playerName, err)
		return
	}
	log.Printf("[Bot %s]: Pacote inicial recebido: %s", playerName, string(p))

	// 3. Ação automatizada: O bot abre 2 pacotes de cartas.
	log.Printf("[Bot %s]: Abrindo 2 pacotes extras...", playerName)
	for i := 0; i < 2; i++ {
		conn.WriteMessage(websocket.TextMessage, []byte("OPEN_PACK"))
		_, p, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[Bot %s]: Erro ao abrir pacote extra: %v", playerName, err)
			break
		}
		log.Printf("[Bot %s]: Pacote extra %d aberto. Resposta: %s", playerName, i+1, string(p))
	}

	// 4. Ação automatizada: O bot entra na fila para uma partida.
	log.Printf("[Bot %s]: Procurando partida...", playerName)
	conn.WriteMessage(websocket.TextMessage, []byte("FIND_MATCH"))

	// 5. Loop principal do bot, que reage às mensagens do servidor.
	for {
		_, p, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[Bot %s]: Conexão perdida: %v", playerName, err)
			break
		}

		message := strings.TrimSpace(string(p))

		if strings.HasPrefix(message, "MATCH_START|") {
			// Ao iniciar a partida, o bot joga a primeira carta ("1") automaticamente.
			log.Printf("[Bot %s]: Partida iniciada! Jogando...", playerName)
			conn.WriteMessage(websocket.TextMessage, []byte("1"))
		} else if strings.HasPrefix(message, "RESULT|") {
			// Ao receber o resultado, o bot encerra sua execução.
			log.Printf("[Bot %s]: Partida finalizada. Resultado: %s", playerName, message)
			break
		} else if message == "NO_MATCH_FOUND" {
			log.Printf("[Bot %s]: Nenhum oponente encontrado. Encerrando.", playerName)
			break
		} else if strings.HasPrefix(message, "TIMER|") {
			// Ignora o TIMER, o bot joga imediatamente após MATCH_START
		} else {
			log.Printf("[Bot %s]: [Servidor]: %s", playerName, message)
		}
	}
	log.Printf("[Bot %s]: Desconectando.", playerName)
}

// handleServerConnection gerencia a lógica para um jogador humano.
func handleServerConnection(playerName string, serverWsUrl string) {
	u, _ := url.Parse(serverWsUrl)
	var conn *websocket.Conn
	var err error

	// Tenta se conectar ao servidor com um número máximo de retentativas.
	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		conn, _, err = websocket.DefaultDialer.Dial(u.String(), nil)
		if err == nil {
			break // Conexão bem-sucedida.
		}
		log.Printf("%s: Falha ao conectar ao servidor (%v). Tentando novamente em 2 segundos...", playerName, err)
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		log.Fatalf("%s: Não foi possível conectar ao servidor após %d tentativas.", playerName, maxRetries)
	}
	defer conn.Close()

	// 1. Envia o nome do jogador
	conn.WriteMessage(websocket.TextMessage, []byte(playerName))
	log.Printf("%s: Conectado com sucesso!", playerName)

	// Contexto para cancelar a leitura de jogada em caso de fim de partida
	_, cancelGame := context.WithCancel(context.Background())
	defer cancelGame()

	// Inicia uma goroutine para ouvir mensagens do servidor de forma assíncrona.
	go listenServerMessages(conn, playerName, cancelGame)

	// Loop principal que lê a entrada do teclado do usuário.
	reader := bufio.NewReader(os.Stdin)
	for {
		stateMutex.Lock()
		canShowMenu := !isSearching && !isInGame
		stateMutex.Unlock()

		if canShowMenu {
			showMenu()
			input, _ := reader.ReadString('\n')
			choice := strings.TrimSpace(input)

			// Envia comandos para o servidor com base na escolha do usuário.
			switch choice {
			case "1":
				stateMutex.Lock()
				isSearching = true // Atualiza o estado para "procurando".
				stateMutex.Unlock()
				conn.WriteMessage(websocket.TextMessage, []byte("FIND_MATCH"))
				go runSearchCountdown(matchmakingTimeoutSeconds) // Inicia o contador visual.
			case "2":
				conn.WriteMessage(websocket.TextMessage, []byte("OPEN_PACK"))
			case "3":
				conn.WriteMessage(websocket.TextMessage, []byte("VIEW_DECK"))
			case "4":
				return // Encerra a função e o programa.
			default:
				fmt.Println("Opção inválida. Tente novamente.")
			}
		}
		time.Sleep(100 * time.Millisecond) // Pausa para evitar uso excessivo de CPU.
	}
}

// showMenu apenas exibe as opções de ação para o jogador.
func showMenu() {
	fmt.Println("\n--- MENU PRINCIPAL ---")
	fmt.Println("1. Procurar Partida")
	fmt.Println("2. Abrir Pacote de Cartas")
	fmt.Println("3. Ver Meu Deck")
	fmt.Println("4. Sair")
	fmt.Print("> ")
}

// listenServerMessages roda em background para processar todas as mensagens recebidas do servidor.
func listenServerMessages(conn *websocket.Conn, playerName string, cancelGame context.CancelFunc) {
	for {
		_, p, err := conn.ReadMessage()
		if err != nil {
			log.Printf("%s: Conexão com o servidor perdida: %v", playerName, err)
			os.Exit(0)
		}

		message := strings.TrimSpace(string(p))
		fmt.Printf("\r%s\n", strings.Repeat(" ", 50)) // Limpa a linha atual antes de exibir a mensagem.

		// Trata as diferentes mensagens do servidor, atualizando o estado do cliente conforme necessário.
		if strings.HasPrefix(message, "MATCH_START|") {
			stateMutex.Lock()
			isSearching = false
			isInGame = true
			stateMutex.Unlock()
			// A lógica de cancelGame precisa ser atualizada para lidar com o contexto do jogo
			handleGame(context.Background(), conn, message)
		} else if strings.HasPrefix(message, "RESULT|") {
			cancelGame() // Cancela a leitura de jogada, se estiver pendente.
			parts := strings.SplitN(message, "|", 2)
			fmt.Printf("\r--- FIM DA PARTIDA ---\n%s\n---------------------\n", parts[1])
			stateMutex.Lock()
			isInGame = false // Retorna ao estado ocioso.
			stateMutex.Unlock()
		} else if message == "MATCH_FOUND" {
			fmt.Printf("\r[Servidor]: Partida encontrada! Iniciando...\n")
			stateMutex.Lock()
			isSearching = false
			stateMutex.Unlock()
		} else if message == "NO_MATCH_FOUND" {
			fmt.Printf("\r[Servidor]: Nenhum oponente encontrado a tempo. Tente novamente.\n")
			stateMutex.Lock()
			isSearching = false // Retorna ao estado ocioso.
			stateMutex.Unlock()
		} else if strings.HasPrefix(message, "TIMER|") {
			parts := strings.Split(message, "|")
			seconds, _ := strconv.Atoi(parts[1])
			go runGameCountdown(seconds) // Inicia o contador de tempo de jogada.
		} else {
			// Exibe qualquer outra mensagem genérica do servidor.
			fmt.Printf("\r[Servidor]: %s\n", message)
		}

		// Se o jogador não estiver ocupado, reexibe o prompt ">" para a próxima ação.
		stateMutex.Lock()
		if !isSearching && !isInGame {
			fmt.Print("> ")
		}
		stateMutex.Unlock()
	}
}

// handleGame exibe a mão do jogador e inicia a captura da sua jogada.
func handleGame(ctx context.Context, conn *websocket.Conn, message string) {
	parts := strings.Split(message, "|")
	card1 := parts[1]
	card2 := parts[2]

	fmt.Println("\r--- PARTIDA INICIADA ---")
	fmt.Println("Sua mão:")
	fmt.Printf("1: %s\n", card1)
	fmt.Printf("2: %s\n", card2)
	fmt.Print("Escolha sua carta (1 ou 2): > ")

	// Inicia a leitura da jogada em uma goroutine para não bloquear o programa.
	go readPlayerInput(ctx, conn)
}

// readPlayerInput gerencia a entrada do jogador durante uma partida.
func readPlayerInput(ctx context.Context, conn *websocket.Conn) {
	choiceChan := make(chan string)
	reader := bufio.NewReader(os.Stdin)

	// Lê a entrada do teclado em uma goroutine separada para não travar.
	go func() {
		input, err := reader.ReadString('\n')
		if err == nil {
			choiceChan <- strings.TrimSpace(input)
		}
	}()

	// O 'select' aguarda por dois eventos simultaneamente:
	select {
	case choice := <-choiceChan:
		conn.WriteMessage(websocket.TextMessage, []byte(choice))
		fmt.Println("Jogada enviada. Aguardando resultado...")
	case <-ctx.Done():
		fmt.Println("\nA partida terminou antes de você fazer uma jogada.")
		return
	}
}

// runSearchCountdown mostra um contador visual enquanto procura uma partida.
func runSearchCountdown(seconds int) {
	for i := seconds; i > 0; i-- {
		stateMutex.Lock()
		if !isSearching {
			stateMutex.Unlock()
			fmt.Printf("\r%s\r", strings.Repeat(" ", 50)) // Limpa a linha.
			return
		}
		stateMutex.Unlock()

		fmt.Printf("\rBuscando partida... Tempo restante: %d segundos ", i)
		time.Sleep(1 * time.Second)
	}
	fmt.Printf("\r%s\r", strings.Repeat(" ", 50))
}

// runGameCountdown mostra um contador visual para o tempo de jogada.
func runGameCountdown(seconds int) {
	for i := seconds; i > 0; i-- {
		stateMutex.Lock()
		if !isInGame {
			stateMutex.Unlock()
			fmt.Printf("\r%s\r", strings.Repeat(" ", 50)) // Limpa a linha.
			return
		}
		stateMutex.Unlock()

		fmt.Printf("\rTempo de jogada restante: %d segundos... ", i)
		time.Sleep(1 * time.Second)
	}
	fmt.Printf("\r%s\r", strings.Repeat(" ", 50))
}
