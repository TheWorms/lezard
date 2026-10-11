#!/bin/sh
# Collecteur Freebox pour Lézard : lit les températures de la box par son API locale
# et les envoie à Lézard (host=freebox). À lancer par cron sur une machine du réseau local.
#
#   lezard-freebox.sh register   # une seule fois : autorisation (bouton sur la box)
#   lezard-freebox.sh            # un relevé (cron chaque minute)
#
# Config : /etc/lezard-freebox.conf (600) avec FREEBOX_URL et FREEBOX_APP_TOKEN
# (écrit par « register »), et la config d'envoi habituelle /etc/lezard-push.conf
# (LEZARD_URL, LEZARD_TOKEN). Dépendances : curl, openssl, awk, sed.
set -eu

FB_CONF="${FREEBOX_CONF:-/etc/lezard-freebox.conf}"
PUSH_CONF="${LEZARD_CONF:-/etc/lezard-push.conf}"
APP_ID="fr.lezard.temp"
# Adresse officielle de l'API locale ; à remplacer par http://IP-DE-LA-BOX si besoin.
FREEBOX_URL="http://mafreebox.freebox.fr"
LEZARD_HOST="freebox"

# shellcheck source=/dev/null
[ -r "$PUSH_CONF" ] && . "$PUSH_CONF"
# shellcheck source=/dev/null
[ -r "$FB_CONF" ] && . "$FB_CONF"
FREEBOX_URL="${FREEBOX_URL%/}"
API="$FREEBOX_URL/api/v4"

die() { echo "lezard-freebox: $*" >&2; exit 1; }
fb() { curl -fsS --max-time 8 "$@"; }
# extrait une valeur simple "clé":"valeur" ou "clé":nombre d'un JSON à plat
# La Freebox écrit les « / » sous la forme « \/ » dans son JSON : on les rétablit (jetons, challenge).
jget() { sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\{0,1\}\([^",}]*\)"\{0,1\}.*/\1/p' | head -n1 | sed 's#\\/#/#g'; }

if [ "${1:-}" = "register" ]; then
  [ "$(id -u)" -eq 0 ] || die "lancer avec sudo (écrit $FB_CONF)"
  out=$(fb -X POST "$API/login/authorize/" -H 'Content-Type: application/json' \
    -d '{"app_id":"'"$APP_ID"'","app_name":"Lezard","app_version":"1.0","device_name":"Lezard"}') || die "Freebox injoignable sur $FREEBOX_URL"
  token=$(printf %s "$out" | jget app_token)
  track=$(printf %s "$out" | jget track_id)
  [ -n "$token" ] && [ -n "$track" ] || die "réponse inattendue de la Freebox"
  echo "Va devant la Freebox et appuie sur la flèche de droite de l'écran de la façade (60 s)."
  i=0
  while [ "$i" -lt 30 ]; do
    st=$(fb "$API/login/authorize/$track" | jget status) || st=""
    case "$st" in
      granted)
        umask 077
        printf 'FREEBOX_URL=%s\nFREEBOX_APP_TOKEN=%s\n' "$FREEBOX_URL" "$token" > "$FB_CONF"
        chmod 600 "$FB_CONF"
        echo "Autorisation accordée, jeton enregistré dans $FB_CONF (non affiché)."
        exit 0 ;;
      denied|timeout|unknown) die "autorisation refusée ou expirée ($st)" ;;
    esac
    i=$((i + 1)); sleep 2
  done
  die "délai dépassé : relance la commande et appuie plus vite sur la flèche"
fi

[ -n "${FREEBOX_APP_TOKEN:-}" ] || die "pas de jeton : lancer d'abord « register »"
[ -n "${LEZARD_URL:-}" ] && [ -n "${LEZARD_TOKEN:-}" ] || die "LEZARD_URL / LEZARD_TOKEN absents de $PUSH_CONF"
# curl 7.55 minimum (en-tête lu sur l'entrée standard) : sinon, l'envoi échouerait en 401 sans explication
curl --version 2>/dev/null | awk 'NR==1 { split($2, v, "."); exit !(v[1] + 0 > 7 || (v[1] + 0 == 7 && v[2] + 0 >= 55)) }' \
  || { echo "lezard-freebox : curl 7.55 ou plus récent requis" >&2; exit 2; }

# Une session neuve par tentative ; la Freebox répond parfois 403 de façon sporadique,
# donc jusqu'à 3 essais. La session est refermée après usage.
sys=""
tries=0
err=""
while [ "$tries" -lt 3 ]; do
  tries=$((tries + 1))
  challenge=$(fb "$API/login/" 2>/dev/null | jget challenge) || challenge=""
  [ -n "$challenge" ] || { err="pas de challenge (API Freebox inattendue)"; sleep 1; continue; }
  pass=$(printf %s "$challenge" | openssl dgst -sha1 -hmac "$FREEBOX_APP_TOKEN" | sed 's/^.*= *//')
  sess=$(fb -X POST "$API/login/session/" -H 'Content-Type: application/json' \
    -d '{"app_id":"'"$APP_ID"'","password":"'"$pass"'"}' 2>/dev/null | jget session_token) || sess=""
  [ -n "$sess" ] || { err="ouverture de session refusée"; sleep 1; continue; }
  resp=$(curl -sS --max-time 8 -w '\n%{http_code}' -H "X-Fbx-App-Auth: $sess" "$API/system/" 2>&1) || resp=""
  code=$(printf %s "$resp" | tail -n1)
  body=$(printf %s "$resp" | sed '$d')
  curl -s --max-time 5 -o /dev/null -X POST -H "X-Fbx-App-Auth: $sess" "$API/login/logout/" || true
  if [ "$code" = "200" ]; then sys=$body; break; fi
  err="/system : HTTP $code, error_code=$(printf %s "$body" | jget error_code), msg=$(printf %s "$body" | jget msg)"
  sleep 1
done
[ -n "$sys" ] || die "$err (après $tries essais ; si droit manquant : Freebox OS > Gestion des accès > Applications > Lezard > Modification des réglages)"

# Deux formes selon le modèle : clés plates temp_cpum / temp_sw..., ou tableau sensors.
vals=$(printf %s "$sys" | tr '{' '\n' | awk '
  /"id"[[:space:]]*:[[:space:]]*"temp_/ && /"value"/ {
    id=$0; sub(/.*"id"[[:space:]]*:[[:space:]]*"temp_/, "", id); sub(/".*/, "", id)
    v=$0;  sub(/.*"value"[[:space:]]*:[[:space:]]*/, "", v); sub(/[^0-9.-].*/, "", v)
    if (id != "" && v != "") print id, v
  }')
flat=$(printf %s "$sys" | tr ',' '\n' | sed -n 's/.*"temp_\([a-z0-9_]*\)"[[:space:]]*:[[:space:]]*\(-\{0,1\}[0-9.]\{1,\}\).*/\1 \2/p')
all=$(printf '%s\n%s\n' "$vals" "$flat" | awk 'NF==2 && !seen[$1]++')
[ -n "$all" ] || die "aucune température trouvée dans la réponse"

printf '%s\n' "$all" | while read -r name val; do
  sensor=$(printf %s "$name" | tr '[:upper:]' '[:lower:]' | tr '_' '-')
  ok=$(awk -v v="$val" 'BEGIN{print (v+0 >= -40 && v+0 <= 150) ? 1 : 0}')
  [ "$ok" = 1 ] || continue
  printf 'Authorization: Bearer %s\n' "$LEZARD_TOKEN" | curl -fsS --max-time 8 -o /dev/null -X POST "$LEZARD_URL/api/push" \
    -H @- \
    -d "host=$LEZARD_HOST" -d "sensor=$sensor" -d "temp=$val" || echo "lezard-freebox: envoi de $sensor échoué" >&2
done
