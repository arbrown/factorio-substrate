module github.com/arbrown/factorio-substrate

go 1.27.0

require (
	github.com/agent-substrate/substrate v0.0.0
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674
	google.golang.org/grpc v1.83.2
)

replace github.com/agent-substrate/substrate => ../substrate
