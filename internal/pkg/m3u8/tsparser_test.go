package m3u8

import (
	"context"
	"testing"
)

// buildTSParserWithFragments constructs a *TSParser backed by a hand-built
// tsStream containing one video PES fragment whose single packet carries a
// non-empty payload. This avoids the flakiness of synthesizing a fully valid
// encrypted TS stream: the test only needs decryptPES to enter its fragment
// loop so the ctx.Err() check can fire.
func buildTSParserWithFragments(t *testing.T) *TSParser {
	t.Helper()
	// A 32-byte payload so there is data to iterate in decryptPES.
	payload := make([]byte, 32)
	byteBuf := make([]byte, 64)
	pkt := &tsPacket{
		header: tsHeader{
			pid: 0x100,
		},
		payloadStartOffset: 0,
		payloadLength:       len(payload),
		payload:             payload,
	}
	frag := &tsPesFragment{packets: []*tsPacket{pkt}}
	stream := &tsStream{
		data:    byteBuf,
		key:     make([]byte, 16),
		videos:  []*tsPesFragment{frag},
		audios:  nil,
	}
	return &TSParser{stream: stream}
}

func TestTSParserDecryptCancelledByCtx(t *testing.T) {
	p := buildTSParserWithFragments(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := p.Decrypt(ctx)
	if err == nil {
		t.Fatal("Decrypt with cancelled ctx should return ctx.Err(), got nil")
	}
}

func TestTSParserDecryptCancelledDirectlyInDecryptPES(t *testing.T) {
	p := buildTSParserWithFragments(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Directly exercise decryptPES with the cancelled ctx over the video
	// fragments to prove the per-fragment ctx.Err() check fires.
	err := p.decryptPES(ctx, p.stream.data, p.stream.videos, p.stream.key)
	if err == nil {
		t.Fatal("decryptPES with cancelled ctx should return ctx.Err(), got nil")
	}
}

func TestTSParserDecryptNormalCtx(t *testing.T) {
	p := buildTSParserWithFragments(t)
	if _, err := p.Decrypt(context.Background()); err != nil {
		t.Fatalf("Decrypt with normal ctx should succeed, got %v", err)
	}
}
