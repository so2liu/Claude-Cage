set -euo pipefail
domain=$1
locale=$2
language=${locale/_/-}

current=$(/usr/bin/defaults read "$domain" AppleLocale 2>/dev/null || true)
if [[ ${current%%@*} != "$locale" ]]; then
  /usr/bin/defaults write "$domain" AppleLocale -string "$locale"
fi

languages=$(/usr/bin/defaults read "$domain" AppleLanguages 2>/dev/null || echo '()')
plist=$(/usr/bin/plutil -convert xml1 -o - - <<< "$languages")
first=$(/usr/bin/plutil -extract 0 raw -o - - <<< "$plist" 2>/dev/null || true)
if [[ "$first" != "$language" ]]; then
  preferred=("$language")
  i=0
  while previous=$(/usr/bin/plutil -extract "$i" raw -o - - <<< "$plist" 2>/dev/null); do
    if [[ "$previous" != "$language" ]]; then preferred+=("$previous"); fi
    i=$((i+1))
  done
  /usr/bin/defaults write "$domain" AppleLanguages -array "${preferred[@]}"
fi
