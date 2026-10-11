#!/usr/bin/env bash
# Crée .env à partir de .env.example : jeton d'envoi généré, IP d'écoute proposée,
# mot de passe admin saisi sans écho. Aucun secret ne passe en argument de commande.
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -e .env ]; then
  echo ".env existe déjà : renommez-le d'abord pour le recréer." >&2
  exit 1
fi
LZ_TOKEN=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')

# IP LAN proposée : la première adresse de l'hôte, sinon 127.0.0.1
suggest=$(hostname -I 2>/dev/null | awk '{print $1}')
suggest=${suggest:-127.0.0.1}
read -rp "IP d'écoute (IP LAN de cet hôte, joignable par les machines) [$suggest] : " LZ_BIND
LZ_BIND=${LZ_BIND:-$suggest}
if ! printf %s "$LZ_BIND" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$'; then
  echo "IP illisible : $LZ_BIND" >&2; exit 1
fi

while :; do
  read -rsp "Mot de passe admin de Lézard (10 caractères min.) : " LZ_ADMIN; echo
  if [ "${#LZ_ADMIN}" -lt 10 ]; then echo "Trop court."; continue; fi
  read -rsp "Confirmation : " LZ_ADMIN2; echo
  if [ "$LZ_ADMIN" = "$LZ_ADMIN2" ]; then break; fi
  echo "Les deux saisies diffèrent."
done
case "$LZ_ADMIN" in *'$'*|*'"'*|*"'"*|*\\*|*' '*)
  echo "Évitez les caractères \$ \" ' \\ et les espaces (lus par docker compose)." >&2; exit 1 ;;
esac
export LZ_TOKEN LZ_ADMIN LZ_BIND
umask 077
awk '
  /^LEZARD_TOKEN=/          { print "LEZARD_TOKEN=" ENVIRON["LZ_TOKEN"]; next }
  /^LEZARD_ADMIN_PASSWORD=/ { print "LEZARD_ADMIN_PASSWORD=" ENVIRON["LZ_ADMIN"]; next }
  /^LEZARD_BIND_IP=/        { print "LEZARD_BIND_IP=" ENVIRON["LZ_BIND"]; next }
  { print }
' .env.example > .env
unset LZ_TOKEN LZ_ADMIN LZ_ADMIN2 LZ_BIND
echo ".env créé (droits 600). Jeton d'envoi des machines : grep LEZARD_TOKEN .env"
