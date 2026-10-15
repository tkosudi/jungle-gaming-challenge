#!/bin/sh
# Cria o .env local a partir do .env.example, trocando cada valor change-me-* por um segredo
# aleatório (openssl rand -hex 24). Os valores não são impressos. Recusa sobrescrever um .env
# existente sem --force; ao trocar os client secrets, recrie o Keycloak (ver README).
set -eu
cd "$(dirname "$0")/.."

if [ -e .env ] && [ "${1:-}" != "--force" ]; then
  echo ".env already exists; pass --force to replace it" >&2
  exit 1
fi

umask 077
tmp=$(mktemp .env.XXXXXX)
trap 'rm -f "$tmp"' EXIT
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    [A-Z]*=change-me-*) printf '%s=%s\n' "${line%%=*}" "$(openssl rand -hex 24)" ;;
    *) printf '%s\n' "$line" ;;
  esac
done < .env.example > "$tmp"
mv "$tmp" .env
trap - EXIT
echo "wrote .env with random local secrets"
