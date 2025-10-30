package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	stockKey = "global_card_stock"
)

// SCRIPT LUA
// Este script é executado atomicamente pelo Redis para cada chamada.
// Ele verifica se há cartas suficientes (3) e, se houver, as remove da lista (LPOP)
// e as retorna. Tudo em uma única operação indivisível.
//
// KEYS[1] = a chave da lista de estoque (stockKey)
// ARGV[1] = o número de cartas por pacote (pack_size = 3)
var atomicOpenPackScript = redis.NewScript(`
    local stock_key = KEYS[1]
    local pack_size = tonumber(ARGV[1])
    
    -- 1. Verifica o tamanho atual da lista
    local current_stock = redis.call('LLEN', stock_key)
    
    -- 2. Se for menor que o tamanho do pacote (3), retorna uma tabela vazia
    if current_stock < pack_size then
        return {}
    end
    
    -- 3. Se houver estoque, remove 'pack_size' (3) cartas do início da lista
    local cards = redis.call('LPOP', stock_key, pack_size)
    
    -- 4. Retorna as cartas (como uma lista de strings JSON)
    return cards
`)

// initializeDistributedStock cria o estoque de cartas no Redis.
func (s *Server) initializeDistributedStock() {
	ctx := context.Background()
	// Verifica se o estoque já existe no Redis.
	count, err := s.RedisClient.LLen(ctx, stockKey).Result()
	if err != nil {
		log.Fatalf("Erro ao verificar estoque no Redis: %v", err)
	}

	if count > 0 {
		log.Printf("Estoque de cartas já existe no Redis. Total de pacotes: %d", count/3)
		return
	}

	// 1. Definição das cartas base
	baseCards := []Card{
		{Name: "Camponês Armado", Forca: 1}, {Name: "Batedor Anão", Forca: 1}, {Name: "Arqueiro Elfo", Forca: 1},
		{Name: "Ghoul", Forca: 1}, {Name: "Nekker", Forca: 1}, {Name: "Infantaria Leve", Forca: 2},
		{Name: "Guerrilheiro Scoia'tael", Forca: 2}, {Name: "Balista", Forca: 2}, {Name: "Lanceiro de Kaedwen", Forca: 3},
		{Name: "Caçador de Recompensa", Forca: 3}, {Name: "Grifo", Forca: 3}, {Name: "Cavaleiro de Aedirn", Forca: 4},
		{Name: "Elemental da Terra", Forca: 4}, {Name: "Guerreiro Anão", Forca: 5}, {Name: "Wyvern", Forca: 5},
		{Name: "Gigante de Gelo", Forca: 6}, {Name: "Leshen", Forca: 6}, {Name: "Grão-Mestre Bruxo", Forca: 7},
		{Name: "Draug", Forca: 7}, {Name: "Ifrit", Forca: 8}, {Name: "Cavaleiro da Morte", Forca: 8},
		{Name: "Behemoth", Forca: 9}, {Name: "Dragão Menor", Forca: 10}, {Name: "Comandante Veterano", Forca: 10},
		{Name: "Eredin Bréacc Glas", Forca: 11}, {Name: "Imlerith", Forca: 11}, {Name: "Vernon Roche", Forca: 12},
		{Name: "Iorveth", Forca: 12}, {Name: "Philippa Eilhart", Forca: 13}, {Name: "Triss Merigold", Forca: 13},
		{Name: "Yennefer de Vengerberg", Forca: 14}, {Name: "Rei Foltest", Forca: 14}, {Name: "Geralt de Rívia", Forca: 15},
	}

	// 2. Cria um grande estoque de cartas (90000 cartas)
	fullCardStock := []Card{}
	for _, card := range baseCards {
		copies := 10 // Padrão para as cartas mais raras (Força > 10)
		if card.Forca >= 1 && card.Forca <= 3 {
			copies = 4000
		} else if card.Forca >= 4 && card.Forca <= 6 {
			copies = 3000
		} else if card.Forca >= 7 && card.Forca <= 10 {
			copies = 2000
		}
		for i := 0; i < copies; i++ {
			fullCardStock = append(fullCardStock, card)
		}
	}

	// Garante que o estoque tenha exatamente 90000 cartas
	for len(fullCardStock) < 90000 {
		fullCardStock = append(fullCardStock, baseCards[0])
	}
	fullCardStock = fullCardStock[:90000]

	// 3. Embaralha o estoque
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(fullCardStock), func(i, j int) {
		fullCardStock[i], fullCardStock[j] = fullCardStock[j], fullCardStock[i]
	})

	// 4. Converte as cartas para JSON e as adiciona ao Redis como uma lista (LIFO - Rpush)
	var cardJsons []interface{}
	for _, card := range fullCardStock {
		cardJson, _ := json.Marshal(card)
		cardJsons = append(cardJsons, string(cardJson))
	}

	// Adiciona todas as cartas ao Redis.
	s.RedisClient.RPush(ctx, stockKey, cardJsons...)

	log.Printf("Estoque de cartas inicializado no Redis. Total de cartas: %d", len(fullCardStock))
}

// openCardPack distribuído: remove um pacote do estoque global (Redis) de forma ATÔMICA.
func (s *Server) openCardPackDistributed(playerName string) ([]Card, error) {
	ctx := context.Background()
	const packSize = 3 // Um pacote tem 3 cartas

	// Executa o script LUA atomicamente
	// KEYS[1] = stockKey
	// ARGV[1] = packSize (3)
	result, err := atomicOpenPackScript.Run(ctx, s.RedisClient, []string{stockKey}, packSize).Result()
	if err != nil {
		// Erro na execução do script
		log.Printf("Servidor %s: Erro ao executar script LUA: %v", s.ServerID, err)
		return nil, fmt.Errorf("erro interno ao processar o estoque: %w", err)
	}

	// 2. Processa o resultado do script
	// O LUA retorna um []interface{} de strings (JSON)
	cardInterfaces, ok := result.([]interface{})
	if !ok {
		log.Printf("Servidor %s: Resultado inesperado do script LUA: %T", s.ServerID, result)
		return nil, fmt.Errorf("erro interno (resultado script)")
	}

	// 3. Verifica se o pacote foi retornado
	// Se o script retornou uma tabela vazia ({}), o estoque acabou.
	if len(cardInterfaces) == 0 {
		log.Printf("Servidor %s: Tentativa de abrir pacote para %s, mas estoque insuficiente.", s.ServerID, playerName)
		return nil, fmt.Errorf("não há pacotes de cartas suficientes no estoque global")
	}

	// 4. Converte JSON para objetos Card e retorna o pacote
	var pack []Card
	for _, cardJSON := range cardInterfaces {
		cardString, isString := cardJSON.(string)
		if !isString {
			log.Printf("Erro crítico ao desserializar carta do Redis: item não é string")
			return nil, fmt.Errorf("erro interno ao processar pacote (item não string)")
		}

		var card Card
		if err := json.Unmarshal([]byte(cardString), &card); err != nil {
			log.Printf("Erro crítico ao desserializar carta do Redis: %v", err)
			return nil, fmt.Errorf("erro interno ao processar pacote (json invalido)")
		}
		pack = append(pack, card)
	}

	return pack, nil
}

// openCardPack é a função que o servidor local chamará.
func (s *Server) openCardPack(player *PlayerState, isMandatory bool) {
	if !isMandatory && player.PacksOpened >= 3 {
		s.sendWebSocketMessage(player, "Você já abriu o máximo de 3 pacotes.")
		return
	}

	pack, err := s.openCardPackDistributed(player.Name)
	if err != nil {
		s.sendWebSocketMessage(player, fmt.Sprintf("Desculpe, %s", err.Error()))
		return
	}

	player.Deck = append(player.Deck, pack...)
	player.PacksOpened++

	// Constrói e envia a resposta ao jogador
	var response string
	if isMandatory {
		response = fmt.Sprintf("Bem-vindo(a), %s! Você recebeu seu pacote inicial: ", player.Name)
	} else {
		response = fmt.Sprintf("Parabéns, %s! Você abriu um pacote extra e recebeu: ", player.Name)
	}
	for i, card := range pack {
		response += fmt.Sprintf("%s (Força: %d)", card.Name, card.Forca)
		if i < len(pack)-1 {
			response += ", "
		}
	}
	// Consulta o estoque restante
	remainingPacks, _ := s.RedisClient.LLen(context.Background(), stockKey).Result()
	response += fmt.Sprintf(". Pacotes restantes no servidor: %d\n", remainingPacks/3)

	s.sendWebSocketMessage(player, response)
}

// viewDeck envia ao jogador uma lista de todas as cartas em seu deck.
func (s *Server) viewDeck(player *PlayerState) {
	if len(player.Deck) == 0 {
		s.sendWebSocketMessage(player, "Seu deck está vazio.")
		return
	}
	response := "Seu deck: "
	for i, card := range player.Deck {
		response += fmt.Sprintf("%s (Força: %d)", card.Name, card.Forca)
		if i < len(player.Deck)-1 {
			response += " | "
		}
	}
	s.sendWebSocketMessage(player, response)
}
