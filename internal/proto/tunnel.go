package proto

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Gateway <-> host tunnel messages (JSON, length-prefixed, on the control
// stream opened by the host over ALPN recon-host/1).

type TunnelMsg struct {
	T string `json:"t"`

	// register (host -> gateway)
	HostID   string        `json:"hostId,omitempty"`
	Token    string        `json:"token,omitempty"`
	Name     string        `json:"name,omitempty"`
	OS       string        `json:"os,omitempty"`
	Version  string        `json:"version,omitempty"`
	MACs     []string      `json:"macs,omitempty"`
	Direct   *DirectInfo   `json:"direct,omitempty"`
	Relay    *RelayInfo    `json:"relay,omitempty"` // also in "direct" updates; absent: no UDP relay (older hosts)
	Encoders []string      `json:"encoders,omitempty"`
	Monitors []MonitorInfo `json:"monitors,omitempty"`

	// registered (gateway -> host)
	DirectKey string `json:"directKey,omitempty"` // base64, HMAC key for direct-path tickets

	// open (gateway -> host): a relay data connection (QUIC splice)
	// relay (gateway -> host): a UDP relay allocation; SID is its ID, Nonce
	// its base64 bind token, Port the gateway's allocation port
	SID   string `json:"sid,omitempty"`
	Nonce string `json:"nonce,omitempty"`
	User  string `json:"user,omitempty"`
	Port  int    `json:"port,omitempty"`

	// status (host -> gateway)
	Streaming bool   `json:"streaming,omitempty"`
	Detail    string `json:"detail,omitempty"`

	Error string `json:"error,omitempty"`
}

// DirectInfo describes the host's direct WebTransport endpoint.
type DirectInfo struct {
	Port   int      `json:"port"`
	Addr   string   `json:"addr,omitempty"`   // advertised host/IP (else: tunnel peer address)
	Hashes []string `json:"hashes,omitempty"` // base64 SHA-256 certificate hashes
}

// RelayInfo describes the host's end of the UDP relay: the WebTransport server
// behind its outbound relay socket, with the direct path's certificate.
type RelayInfo struct {
	Hashes []string `json:"hashes"` // base64 SHA-256 certificate hashes
}

// DataHello authenticates a per-session data connection (ALPN recon-data/1).
type DataHello struct {
	HostID string `json:"hostId"`
	SID    string `json:"sid"`
	Nonce  string `json:"nonce"`
}

// DirectTicket authorises a browser on the host's direct endpoint. It is
// HMAC-signed by the gateway with the per-connection DirectKey.
type DirectTicket struct {
	HostID string `json:"h"`
	User   string `json:"u"`
	Exp    int64  `json:"exp"`
	Nonce  string `json:"n"`
	Origin string `json:"o"`           // page origin the ticket was issued to; must match the WebTransport Origin header
	Relay  string `json:"r,omitempty"` // UDP relay allocation the session must arrive through ("" = direct path only)
}

// PairingCode is shown by the gateway when a host is added and consumed by
// `recon-host pair`.
type PairingCode struct {
	Gateway string `json:"g"`
	HostID  string `json:"id"`
	Token   string `json:"tok"`
	Pin     string `json:"pin"`
	Name    string `json:"n,omitempty"`
}

const pairingPrefix = "recon1:"

// Encode serialises a pairing code.
func (p PairingCode) Encode() string {
	b, _ := json.Marshal(p)
	return pairingPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParsePairingCode decodes a code produced by the gateway.
func ParsePairingCode(s string) (PairingCode, error) {
	var p PairingCode
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, pairingPrefix) {
		return p, errors.New("not a Recon pairing code")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, pairingPrefix))
	if err != nil {
		return p, fmt.Errorf("pairing code: %w", err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("pairing code: %w", err)
	}
	if p.Gateway == "" || p.HostID == "" || p.Token == "" || p.Pin == "" {
		return p, errors.New("pairing code is incomplete")
	}
	return p, nil
}
