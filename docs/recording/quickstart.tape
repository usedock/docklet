# Quickstart recording for the READMEs. Generated in CI by .github/workflows/recording.yml.
# Everything on screen is a real command with its real output; the workflow pre-pulls the
# ubuntu-24.04 image and removes any earlier dock install before recording, so the timing is honest
# apart from that one download. Render locally: vhs docs/recording/quickstart.tape
Output docs/assets/quickstart.gif
Output docs/assets/quickstart.mp4

Set Shell "bash"
Set FontFamily "DejaVu Sans Mono"
Set FontSize 18
Set Width 1200
Set Height 600
Set Padding 28
Set Framerate 15
Set TypingSpeed 30ms
Set LineHeight 1.25
Set WindowBar Colorful
Set WindowBarSize 36
Set BorderRadius 8
Set Margin 0
Set Theme { "name": "usedock", "black": "#1c1917", "red": "#b91c1c", "green": "#15803d", "yellow": "#a16207", "blue": "#1d4ed8", "magenta": "#7e22ce", "cyan": "#0e7490", "white": "#d6d3d1", "brightBlack": "#78716c", "brightRed": "#dc2626", "brightGreen": "#16a34a", "brightYellow": "#ca8a04", "brightBlue": "#2563eb", "brightMagenta": "#9333ea", "brightCyan": "#0891b2", "brightWhite": "#fafaf9", "background": "#faf7f2", "foreground": "#1c1917", "selection": "#e7e5e4", "cursor": "#c2410c" }

Hide
Type `export PATH="$HOME/.local/bin:$PATH"; export PS1='\[\e[38;2;194;65;12m\]$\[\e[0m\] '` Enter
Type "clear" Enter
Show

Sleep 800ms
Type "curl -fsSL https://usedock.io/install | sh" Enter
Wait+Screen@120s /installed dock/
Sleep 800ms

Type "clear" Enter
Type "dock doctor" Enter
Wait+Screen@60s /try: dock new/
Sleep 800ms

Type "clear" Enter
Type "dock new api" Enter
Wait+Screen@180s /created api/
Sleep 800ms

Type "dock exec api -- uname -a" Enter
Wait+Screen@60s /GNU\/Linux/
Sleep 500ms
Type "dock exec api -- docker --version" Enter
Wait+Screen@60s /Docker version/
Sleep 800ms

Type "clear" Enter
Type "dock branch api --count 2" Enter
Wait+Screen@180s /(?s)branched api.*branched api/
Sleep 500ms
Type "dock ls" Enter
Wait+Screen@60s /STATE/
Sleep 3s
