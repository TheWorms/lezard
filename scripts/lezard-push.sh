#!/bin/sh
# Envoie la température CPU à Lézard. À lancer chaque minute par cron.
# Configuration : /etc/lezard-push.conf (ou ~/.lezard-push.conf, ou /storage/.lezard-push.conf
# sur CoreELEC/LibreELEC), droits 600 :
#   LEZARD_URL=http://IP-DE-LEZARD:3070
#   LEZARD_TOKEN=...
#   LEZARD_HOST=nas             (facultatif, sinon le nom de la machine en minuscules)
#   THERMAL_TYPE=x86_pkg_temp   (facultatif : choisit la zone par son type, stable d'un redémarrage à l'autre)
#   THERMAL_ZONE=/sys/class/thermal/thermal_zone0/temp   (facultatif : chemin exact, prioritaire)
# Le jeton passe à curl par l'entrée standard : il n'apparaît pas dans la liste des processus.
for f in /etc/lezard-push.conf "$HOME/.lezard-push.conf" /storage/.lezard-push.conf; do
  # shellcheck source=/dev/null
  [ -r "$f" ] && . "$f" && break
done
[ -n "$LEZARD_URL" ] && [ -n "$LEZARD_TOKEN" ] || { echo "lezard-push : LEZARD_URL / LEZARD_TOKEN manquants" >&2; exit 2; }
# curl 7.55 minimum (en-tête lu sur l'entrée standard) : sinon, l'envoi échouerait en 401 sans explication
curl --version 2>/dev/null | awk 'NR==1 { split($2, v, "."); exit !(v[1] + 0 > 7 || (v[1] + 0 == 7 && v[2] + 0 >= 55)) }' \
  || { echo "lezard-push : curl 7.55 ou plus récent requis" >&2; exit 2; }

root="${THERMAL_ROOT:-/sys/class/thermal}"
zone="$THERMAL_ZONE"
if [ -z "$zone" ] && [ -n "$THERMAL_TYPE" ]; then
  for d in "$root"/thermal_zone*; do
    if [ "$(cat "$d/type" 2>/dev/null)" = "$THERMAL_TYPE" ]; then zone="$d/temp"; break; fi
  done
  [ -n "$zone" ] || { echo "lezard-push : aucune zone de type $THERMAL_TYPE" >&2; exit 1; }
fi
zone="${zone:-$root/thermal_zone0/temp}"
raw=$(cat "$zone" 2>/dev/null) || exit 1
case "$raw" in ''|*[!0-9-]*) exit 1 ;; esac
# millidegrés -> degrés avec une décimale
temp=$(awk -v r="$raw" 'BEGIN{ if (r > 1000 || r < -1000) r = r/1000; printf "%.1f", r }')
# relevés absurdes (ex. zone acpitz fantôme à -263 °C) ignorés
awk -v t="$temp" 'BEGIN{ exit !(t > -40 && t < 150) }' || exit 1

host="${LEZARD_HOST:-$(hostname | tr '[:upper:]' '[:lower:]')}"
printf 'Authorization: Bearer %s\n' "$LEZARD_TOKEN" | curl -fsS -m 8 -X POST -H @- \
  -d "host=$host" -d "temp=$temp" \
  "$LEZARD_URL/api/push" >/dev/null
