module github.com/karamkamal1/kloudit-recon

go 1.26.0

require (
	github.com/coder/websocket v1.8.15
	github.com/klauspost/reedsolomon v1.14.2
	github.com/quic-go/quic-go v0.63.0
	github.com/quic-go/webtransport-go v0.13.0
	github.com/thesyncim/gopus v0.1.2
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
	rsc.io/qr v0.2.0
)

require (
	github.com/dunglas/httpsfv v1.1.1 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/quic-go/quic-go => ./third_party/quic-go
