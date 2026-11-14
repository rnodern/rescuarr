#!/usr/bin/env bash
set -euo pipefail

mkdir "$1/tls" "$1/media"
openssl req -new -x509 -newkey rsa:2048 -nodes -sha256 -days 2 \
  -subj '/CN=Rescuarr disposable lab CA' \
  -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -keyout "$1/tls/root.key" -out "$1/tls/root.pem"
openssl req -new -newkey rsa:2048 -nodes -sha256 \
  -subj '/CN=skyhook.sonarr.tv' -keyout "$1/tls/source.key" -out "$1/tls/source.csr"
cat > "$1/tls/leaf.ext" <<'EXT'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:skyhook.sonarr.tv,DNS:api.radarr.video
EXT
openssl x509 -req -in "$1/tls/source.csr" -CA "$1/tls/root.pem" -CAkey "$1/tls/root.key" \
  -CAcreateserial -days 2 -sha256 -extfile "$1/tls/leaf.ext" -out "$1/tls/source.pem"
rm "$1/tls/root.key" "$1/tls/source.csr" "$1/tls/root.srl" "$1/tls/leaf.ext"

# One long, low-bitrate video generated from filters, with no input files or network.
ffmpeg -nostdin -hide_banner -loglevel error \
  -f lavfi -i 'color=color=black:size=1280x720:rate=1' \
  -f lavfi -i 'anullsrc=r=48000:cl=stereo' -t 900 -threads 1 \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -b:a 96k \
  -metadata:s:a:0 language=eng -fflags +bitexact -flags:v +bitexact -flags:a +bitexact \
  "$1/media/synthetic.mkv"
# The enclosing temporary directory remains inaccessible to other host users.
chmod 755 "$1/tls" "$1/media"
chmod 644 "$1/tls/root.pem" "$1/tls/source.key" "$1/tls/source.pem" "$1/media/synthetic.mkv"
