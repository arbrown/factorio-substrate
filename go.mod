module github.com/arbrown/factorio-substrate

go 1.27.0

require (
	github.com/agent-substrate/substrate v0.0.0-00010101000000-000000000000
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674
	google.golang.org/grpc v1.84.0
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260720211330-0afa2a65878a // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/agent-substrate/substrate => ../substrate
