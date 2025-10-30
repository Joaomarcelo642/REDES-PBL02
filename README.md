# Projeto de Arquitetura Distribuída - Jogo de Cartas (TEC502 - P2)

Este projeto implementa um jogo de cartas simples em uma arquitetura distribuída, utilizando múltiplos servidores de jogo, Redis para estado compartilhado e mecanismos de comunicação inter-servidores (REST) e cliente-servidor (WebSocket/Pub-Sub).

## Funcionalidades Implementadas (Baseado no Barema de Avaliação)

| Item | Descrição | Implementação |
| :--- | :--- | :--- |
| **1. Arquitetura Distribuída** | Migração de centralizada para distribuída. | Múltiplos serviços `server` (`server-1`, `server-2`) orquestrados pelo Docker Compose, compartilhando estado via Redis. |
| **2. Comunicação Servidor-Servidor** | Protocolo baseado em API REST. | Endpoint `/api/v1/match/notify` para notificação de pareamento e `/api/v1/stock/take` para gerenciamento de estoque. |
| **3. Comunicação Cliente-Servidor** | Protocolo baseado em modelo Publisher-Subscriber. | Utilização de **WebSockets** (`github.com/gorilla/websocket`) para comandos em tempo real (ex: jogar) e **Redis Pub/Sub** para envio de mensagens assíncronas (ex: resultado do jogo, conclusão de troca). |
| **4. Gerenciamento Distribuído de Estoque** | Controle de concorrência para aquisição de pacotes. | Implementação de **Script LUA Atômico** no Redis (`LPOP` múltiplo) para garantir a retirada de pacotes da lista (`global_card_stock`) de forma atômica e segura. |
| **6. Pareamento em Ambiente Distribuído** | Pareamento de jogadores conectados a servidores distintos. | Utilização de **Redis Sorted Set (ZSET)** como fila de matchmaking global e um **Distributed Lock (SETNX)** para o matchmaker, garantindo pareamento único. Comunicação via REST para notificar o servidor do oponente. |
| **7. Sistema de Troca Distribuída** | Troca de cartas assíncrona entre jogadores de servidores distintos. | Utilização de uma **Fila Global (Redis LIST)** para `TradeTickets` (Jogador + Carta) e um **Distributed Lock (SETNX)** para garantir a atomicidade da troca. O retorno da troca ao jogador original é feito via **Redis Pub/Sub**. |
| **8. Testes de Software** | Teste de concorrência distribuída e cenários de falha. | Script `run_tests.sh` e programa `test_concurrency.go` para simular 100 bots e testar a robustez do Distributed Lock e a tolerância a falhas. |
| **9. Emprego do Docker e Emulação Realista** | Desenvolvimento e teste em contêineres Docker. | Arquivos `Dockerfile.server`, `Dockerfile.client` e `docker-compose.yml` para orquestrar 2 servidores, 1 Redis e 1 contêiner de bots. |
| **10. Documentação e Qualidade do Produto** | Código-fonte devidamente comentado e organizado. | O código foi modularizado em arquivos (`server.go`, `models.go`, `stock.go`, `websocket.go`, `matchmaker.go`, `game.go`, `trade.go`) com comentários detalhados. |

**Itens 5 e 7 (Originais):**
* **5. Consistência e Justiça do Estado do Jogo:** O estado crítico (estoque de cartas, fila de trocas) é garantido pelos mecanismos de concorrência do Redis. O estado do jogo (partida) é gerenciado pelo servidor P1 e comunicado via Redis (HSET + Pub/Sub), garantindo consistência entre os dois servidores durante o duelo.
* **7. Tolerância a Falhas e Resiliência:** O script de teste (`run_tests.sh`) inclui um cenário de falha (parar `server-2`) para demonstrar que o sistema (via `server-1`) continua operando. A arquitetura distribuída com Redis (como serviço externo) e a lógica de Distributed Lock (que libera o lock após timeout) contribuem para a resiliência.

## Estrutura do Projeto

```
.
├── docker-compose.yml
├── run_tests.sh
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
    ├── trade.go
    ├── go.mod
    └── Dockerfile.server
```

## Como Executar

**Pré-requisitos:** Docker e Docker Compose instalados.

1.  **Navegue até o diretório raiz do projeto:**
    ```bash
    cd /.../project
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
    * `server-1` é o nome do serviço no Docker Compose, que resolve para o IP interno.

3.  **Inicie um segundo cliente interativo (Jogador B) no `server-2`:**
    ```bash
    docker-compose exec bots /client server-2 JogadorB
    ```

4.  **Teste o pareamento distribuído:**
    * No **Jogador A**, digite `1` (Procurar Partida).
    * No **Jogador B**, digite `1` (Procurar Partida).
    * Os servidores se comunicarão para iniciar a partida.

5.  **Teste a troca de cartas:**
    * Após a partida, no **Jogador A**, digite `3` (Ver Meu Deck) para ver suas cartas.
    * Digite `4` (Trocar Carta) e escolha o número de uma carta (ex: `1`). O servidor confirmará que a carta está na fila.
    * No **Jogador B**, digite `4` (Trocar Carta) e escolha o número de uma carta (ex: `1`).
    * O **Jogador B** receberá a notificação de troca imediatamente.
    * O **Jogador A** receberá a notificação da troca via Pub/Sub (pode levar 1-2 segundos).
    * Ambos podem digitar `3` (Ver Meu Deck) para confirmar que receberam a carta nova.

6.  **Teste o estoque distribuído:**
    * Em ambos os clientes, digite `2` (Abrir Pacote de Cartas) repetidamente para testar a retirada atômica do estoque.

7.  **Limpeza:**
    ```bash
    docker-compose down
    ```
