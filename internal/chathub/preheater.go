package chathub

import (
	"context"
	"sync"

	"github.com/gorilla/websocket"
)

type Preheater struct {
	mu sync.Mutex
}

func NewPreheater() *Preheater {
	return &Preheater{}
}

func (p *Preheater) Take(oid, tid string) *websocket.Conn { return nil }

func (p *Preheater) Warm(ctx context.Context, oid, tid, wsURL string) {}

func (p *Preheater) Stats() map[string]any { return map[string]any{"mode": "stub"} }
