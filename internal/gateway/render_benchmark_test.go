package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

// The reference retains the previous concatenate + full []rune scan on each
// delta. It isolates the allocation change; it is not an end-to-end RSS claim.
func BenchmarkRenderDelta(b *testing.B) {
	for _, size := range []int{20000, 100000} {
		for _, baseline := range []bool{true, false} {
			name := "buffered"
			if baseline { name = "previous-algorithm" }
			b.Run(fmt.Sprintf("%s/%dB", name, size), func(b *testing.B) {
				delta := strings.Repeat("中", 8)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for i:=0; i<b.N; i++ {
					if baseline {
						text, count := "", 0
						for n:=0; n<size; n+=len(delta) {
							old := text
							text = previousRenderCut(old + delta, 60000)
							count -= len([]rune(old))
							count += len([]rune(text))
						}
						if count == 0 { b.Fatal("missing content") }
					} else {
						// A long-lived existing timer models coalescing while the
						// transport is slow, without measuring timer creation.
						r := &render{g:&Gateway{},ctx:context.Background(),blocks:map[string]*renderBlock{},thinking:map[string]string{},timer:time.NewTimer(time.Hour)}
						for n:=0; n<size; n+=len(delta) { r.update(delta,true,"text","a") }
						r.timer.Stop()
						if r.totalChars == 0 { b.Fatal("missing content") }
					}
				}
			})
		}
	}
}

func BenchmarkReplyCard(b *testing.B) {
	answer := strings.Repeat("中文 emoji 🐱 \"quoted\"\n",5000)
	b.ReportAllocs()
	for i:=0;i<b.N;i++ {
		card := replyCard("回复",answer,[]string{"正在分析"},1,true,replyDetails{Model:"example"},time.Second,true)
		n,err := core.CardPayloadBytes(card)
		if err!=nil || n>core.CardPayloadLimit { b.Fatal(n,err) }
	}
}

func previousRenderCut(s string, n int) string {
	if n <= 0 { return "" }
	if len(s) <= n { return s }
	runes := []rune(s)
	if len(runes) <= n { return s }
	return string(runes[:n])
}
