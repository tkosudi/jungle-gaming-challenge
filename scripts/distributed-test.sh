#!/bin/sh
# Teste com três processos reais do sistema operacional (tests/distributed_test.go): compila
# cmd/api com -race e executa os processos contra o PostgreSQL, o Keycloak e o LocalStack do
# Compose (docker compose up -d --wait postgres keycloak localstack). Credenciais vêm do
# ambiente ou do .env. PIDs, resumo (summary.txt) e logs JSON de cada processo ficam em
# DISTRIBUTED_EVIDENCE_DIR (padrão: diretório temporário novo).
set -eu
cd "$(dirname "$0")/.."

DISTRIBUTED_EVIDENCE_DIR=${DISTRIBUTED_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/jungle-distributed-XXXXXX")}
export DISTRIBUTED_EVIDENCE_DIR
mkdir -p "$DISTRIBUTED_EVIDENCE_DIR"
echo "evidence directory: $DISTRIBUTED_EVIDENCE_DIR"

status=0
go test -race -count=1 -tags=integration,distributed -run '^TestDistributed_' -v -timeout 15m ./tests/ \
  > "$DISTRIBUTED_EVIDENCE_DIR/go-test.log" 2>&1 || status=$?
cat "$DISTRIBUTED_EVIDENCE_DIR/go-test.log"
exit "$status"
