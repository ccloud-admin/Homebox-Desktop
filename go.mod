module homebox-desktop

go 1.24

require (
	github.com/wailsapp/wails/v2 v2.11.0
	golang.org/x/sys v0.30.0
)

require (
	github.com/bep/debounce v1.2.1 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jchv/go-winloader v0.0.0-20210711035445-715c2860da7e // indirect
	github.com/leaanthony/go-ansi-parser v1.6.1 // indirect
	github.com/leaanthony/gosod v1.0.4 // indirect
	github.com/leaanthony/slicer v1.6.0 // indirect
	github.com/leaanthony/u v1.1.1 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/samber/lo v1.49.1 // indirect
	github.com/tkrajina/go-reflector v0.5.8 // indirect
	github.com/wailsapp/go-webview2 v1.0.22 // indirect
	github.com/wailsapp/mimetype v1.4.1 // indirect
	golang.org/x/net v0.35.0 // indirect
	golang.org/x/text v0.22.0 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
)

// All dependencies are kept in third_party/ (see scripts/fetch-deps.sh) so
// the app builds without a Go module proxy.
replace (
	github.com/bep/debounce => ./third_party/github.com/bep/debounce
	github.com/go-ole/go-ole => ./third_party/github.com/go-ole/go-ole
	github.com/google/uuid => ./third_party/github.com/google/uuid
	github.com/jchv/go-winloader => ./third_party/github.com/jchv/go-winloader
	github.com/leaanthony/go-ansi-parser => ./third_party/github.com/leaanthony/go-ansi-parser
	github.com/leaanthony/gosod => ./third_party/github.com/leaanthony/gosod
	github.com/leaanthony/slicer => ./third_party/github.com/leaanthony/slicer
	github.com/leaanthony/u => ./third_party/github.com/leaanthony/u
	github.com/pkg/browser => ./third_party/github.com/pkg/browser
	github.com/pkg/errors => ./third_party/github.com/pkg/errors
	github.com/samber/lo => ./third_party/github.com/samber/lo
	github.com/tkrajina/go-reflector => ./third_party/github.com/tkrajina/go-reflector
	github.com/wailsapp/go-webview2 => ./third_party/github.com/wailsapp/go-webview2
	github.com/wailsapp/mimetype => ./third_party/github.com/wailsapp/mimetype
	github.com/wailsapp/wails/v2 => ./third_party/github.com/wailsapp/wails/v2
	golang.org/x/net => ./third_party/golang.org/x/net
	golang.org/x/sys => ./third_party/golang.org/x/sys
	golang.org/x/text => ./third_party/golang.org/x/text
	github.com/godbus/dbus/v5 => ./third_party/github.com/godbus/dbus/v5
	github.com/rivo/uniseg => ./third_party/github.com/rivo/uniseg
)
