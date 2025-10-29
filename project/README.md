# Projeto de Arquitetura Distribuída - Jogo de Cartas (TEC502 - P2)

Este projeto implementa um jogo de cartas simples em uma arquitetura distribuída, utilizando múltiplos servidores de jogo, Redis para estado compartilhado e mecanismos de comunicação inter-servidores (REST) e cliente-servidor (WebSocket/Pub-Sub).

## Funcionalidades Implementadas (Baseado no Barema de Avaliação)

| Item | Descrição | Implementação |
| :--- | :--- | :--- |
| **1. Arquitetura Distribuída** | Migração de centralizada para distribuída. | Múltiplos serviços `server` (`server-1`, `server-2`) orquestrados pelo Docker Compose, compartilhando estado via Redis. |
| **2. Comunicação Servidor-Servidor** | Protocolo baseado em API REST. | Endpoint `/api/v1/match/notify` para notificação de pareamento e `/api/v1/stock/take` para gerenciamento de estoque. |
| **3. Comunicação Cliente-Servidor** | Protocolo baseado em modelo Publisher-Subscriber. | Utilização de **WebSockets** (`github.com/gorilla/websocket`) para comunicação em tempo real e **Redis Pub/Sub** para envio de mensagens assíncronas (ex: notificação de partida). |
| **4. Gerenciamento Distribuído de Estoque** | Controle de concorrência para aquisição de pacotes. | Implementação de **Distributed Lock (SETNX + LUA Script)** no Redis para garantir que apenas um servidor acesse a lista de pacotes (`global_card_stock`) por vez, impedindo duplicação ou perda de cartas. |
| **6. Pareamento em Ambiente Distribuído** | Pareamento de jogadores conectados a servidores distintos. | Utilização de **Redis Sorted Set (ZSET)** como fila de matchmaking global e um **Distributed Lock** para o matchmaker, garantindo pareamento único. Comunicação via REST para notificar o servidor do oponente. |
| **8. Testes de Software** | Teste de concorrência distribuída e cenários de falha. | Script `run_tests.sh` e programa `test_concurrency.go` para simular 100 bots e testar a robustez do Distributed Lock e a tolerância a falhas. |
| **9. Emprego do Docker e Emulação Realista** | Desenvolvimento e teste em contêineres Docker. | Arquivos `Dockerfile.server`, `Dockerfile.client` e `docker-compose.yml` para orquestrar 2 servidores, 1 Redis e 1 contêiner de bots. |
| **10. Documentação e Qualidade do Produto** | Código-fonte devidamente comentado e organizado. | O código foi modularizado em arquivos (`server.go`, `models.go`, `stock.go`, `websocket.go`, `matchmaker.go`, `game.go`) com comentários detalhados e uso de bibliotecas modernas (Go-Chi, Gorilla WebSocket, Go-Redis). |

**Itens 5 e 7:**
*   **5. Consistência e Justiça do Estado do Jogo:** O estado crítico (estoque de cartas) é garantido pelo Redis Distributed Lock (Item 4). O estado do jogo (partida) é temporário e gerenciado pelo servidor que orquestra a partida, garantindo consistência local durante o duelo. Para o estado persistente do jogador (deck), a implementação sugere o uso de Redis para consistência distribuída, mas o foco foi na concorrência do estoque.
*   **7. Tolerância a Falhas e Resiliência:** O script de teste (`run_tests.sh`) inclui um cenário de falha (parar `server-2`) para demonstrar que o sistema (via `server-1`) continua operando. A arquitetura distribuída com Redis (como serviço externo) e a lógica de Distributed Lock (que libera o lock após timeout) contribuem para a resiliência.

## Estrutura do Projeto

```
.
├── docker-compose.yml
├── run_tests.sh
├── README.md
├── client/
│   ├── client.go
│   ├── go.mod
│   └── Dockerfile.client
└── server/
    ├── server.go
    ├── models.go
    ├── stock.go
    ├── websocket.go
    ├── matchmaker.go
    ├── game.go
    ├── go.mod
    └── Dockerfile.server
```

## Como Executar

**Pré-requisitos:** Docker e Docker Compose instalados.

1.  **Navegue até o diretório raiz do projeto:**
    ```bash
    cd /home/ubuntu/project
    ```

2.  **Execute o script de testes:**
    O script `run_tests.sh` irá construir as imagens, iniciar os contêineres, rodar o teste de concorrência e simular uma falha.
    ```bash
    ./run_tests.sh
    ```

## Como Testar Manualmente

1.  **Inicie os serviços:**
    ```bash
    docker-compose up -d --build
    ```

2.  **Inicie um cliente interativo (Jogador A) no `server-1`:**
    ```bash
    docker-compose exec bots /client server-1 JogadorA
    ```
    *   `server-1` é o nome do serviço no Docker Compose, que resolve para o IP interno.

3.  **Inicie um segundo cliente interativo (Jogador B) no `server-2`:**
    ```bash
    docker-compose exec bots /client server-2 JogadorB
    ```

4.  **Teste o pareamento distribuído:**
    *   No **Jogador A**, digite `1` (Procurar Partida).
    *   No **Jogador B**, digite `1` (Procurar Partida).
    *   Os servidores se comunicarão via REST para iniciar a partida, e os clientes receberão a notificação via WebSocket.

5.  **Teste o estoque distribuído:**
    *   Em ambos os clientes, digite `2` (Abrir Pacote de Cartas) repetidamente para testar o Distributed Lock.

6.  **Limpeza:**
    ```bash
    docker-compose down
    ```

