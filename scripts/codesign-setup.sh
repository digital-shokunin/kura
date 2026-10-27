#!/bin/sh
# Create a self-signed code-signing identity in the login keychain.
#
# The macOS Keychain grants access to an item based on the code signature of
# the program that created it. Signing every build with the same certificate
# keeps the item's trusted-application entry (the designated requirement)
# stable, and git-remote-kura, a symlink to the same binary, matches it too.
# Without an Apple Team ID the item's partition list still pins the exact
# build (cdhash), so each new build asks once; answer "Always Allow".
set -eu

NAME="${1:-kura-codesign}"
KEYCHAIN="$HOME/Library/Keychains/login.keychain-db"

if security find-certificate -c "$NAME" "$KEYCHAIN" >/dev/null 2>&1; then
	echo "Code-signing identity '$NAME' already exists."
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/req.cnf" <<EOF
[req]
distinguished_name = dn
prompt = no
[dn]
CN = $NAME
[ext]
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, codeSigning
basicConstraints = critical, CA:false
EOF

# LibreSSL (/usr/bin/openssl) writes PKCS#12 files that `security` accepts.
/usr/bin/openssl req -x509 -newkey rsa:3072 -nodes -days 3650 \
	-keyout "$tmp/key.pem" -out "$tmp/cert.pem" \
	-config "$tmp/req.cnf" -extensions ext 2>/dev/null
pass=$(/usr/bin/openssl rand -hex 16)
/usr/bin/openssl pkcs12 -export -name "$NAME" \
	-inkey "$tmp/key.pem" -in "$tmp/cert.pem" \
	-out "$tmp/identity.p12" -passout "pass:$pass"
security import "$tmp/identity.p12" -k "$KEYCHAIN" -P "$pass" -T /usr/bin/codesign >/dev/null
echo "Created code-signing identity '$NAME' in the login keychain."
