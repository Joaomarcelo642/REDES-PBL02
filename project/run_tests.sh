#!/bin/bash

# Script de Teste de Concorrência e Emulação de Falhas
# Item 8: Testes de Software

set -e

echo "--- 1. CONSTRUINDO IMAGENS DOCKER ---"
docker-compose -f docker-compose.yml build

echo "--- 2. INICIANDO SERVIÇOS (2 Servidores e Redis) ---"
# Inicia apenas os serviços de infraestrutura
docker-compose -f docker-compose.yml up -d redis server-1 server-2

echo "Aguardando 10 segundos para os servidores inicializarem o estoque..."
sleep 10

echo "--- 3. TESTE DE CONCORRÊNCIA DE ABERTURA DE PACOTES ---"
# Garante que o serviço de bots esteja rodando (necessário para `docker-compose exec bots ...`)
docker-compose -f docker-compose.yml up -d bots
BOT_CONTAINER_ID=$(docker-compose -f docker-compose.yml ps -q bots)
if [ -n "$BOT_CONTAINER_ID" ]; then
  echo "Copiando test_concurrency.go para o container bots (${BOT_CONTAINER_ID})..."
  docker cp ./test_concurrency.go ${BOT_CONTAINER_ID}:/app/test_concurrency.go || true
fi

# Compila o bot de teste no container de bots.
# Usa sh -c e caminho relativo (./test_concurrency.go) dentro do container para
# evitar que o Go interprete caminhos do Windows (ex: C:/...) como import paths.
# CORREÇÃO: Adicionado // para evitar conversão de caminho do MINGW
docker-compose -f docker-compose.yml exec bots sh -c "cd /app && go build -o //test_concurrency ./test_concurrency.go"

echo "Executando o teste de concorrência com 10 bots simultâneos..."
# Executa o teste de concorrência dentro do container
# CORREÇÃO: Adicionado // para evitar conversão de caminho do MINGW
docker-compose -f docker-compose.yml exec bots sh -c "//test_concurrency"

echo "--- 4. TESTE DE TOLERÂNCIA A FALHAS (Item 7) ---"
echo "Parando o Servidor 2 (server-2) para simular falha..."
docker-compose -f docker-compose.yml stop server-2

echo "Aguardando 5 segundos..."
sleep 5

echo "Iniciando 10 novos bots para testar se o sistema continua funcional..."
# Executa 10 bots no server-1 (o único que sobrou)
# CORREÇÃO: Adicionado // para evitar conversão de caminho do MINGW
docker-compose -f docker-compose.yml exec bots //client -bot -count=10 -prefix=FailTest server-1

echo "--- 5. LIMPEZA ---"
docker-compose -f docker-compose.yml down

echo "--- TESTES CONCLUÍDOS ---"
echo "Verifique o log do 'server-1' e 'server-2' para análise detalhada."
echo "O teste de concorrência é considerado bem-sucedido se o número de pacotes abertos for próximo a 300 (100 bots * 3 pacotes)."
