package app

import (
	"strings"
	"testing"
	"time"
)

func TestChatSpendSplitsHistoryAndDocuments(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	a.Engine.FetchAttachment = fakeFetch
	onboard(t, a, fc, ctx)
	fm.mu.Lock()
	fm.pdfText = strings.Repeat("Revenue rose. ", 200) // 2,800 characters
	fm.mu.Unlock()
	if err := a.Settings.Set(ctx, "document_chars", "1000"); err != nil {
		t.Fatal(err)
	}
	if err := a.Settings.Set(ctx, "document_chars", "200000"); err == nil {
		t.Fatal("document_chars over the limit was accepted")
	}

	a.Engine.HandleIncoming(ctx, pdfMessage("summarise this"))
	fc.take()
	a.Engine.HandleIncoming(ctx, dm("and the takeaway?"))
	fc.take()

	got := allUserContent(lastChatReq(fm))
	if !strings.Contains(got, "[rest of the document not kept]") || strings.Count(got, "Revenue rose.") > 80 {
		t.Fatalf("document_chars didn't limit the kept text:\n%s", got)
	}
	mix, err := a.DB.PromptMixSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if mix.Calls == 0 || mix.HistoryTokens == 0 || mix.DocTokens == 0 {
		t.Fatalf("chat calls weren't measured: %+v", mix)
	}
}

func TestDocumentKeepingOff(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	a.Engine.FetchAttachment = fakeFetch
	onboard(t, a, fc, ctx)
	a.Settings.Set(ctx, "document_chars", "0")
	a.Engine.HandleIncoming(ctx, pdfMessage("summarise this"))
	fc.take()
	if fm.parseCalls != 0 {
		t.Fatalf("with keeping off, PDFs were parsed for keeping %d times", fm.parseCalls)
	}
	a.Engine.HandleIncoming(ctx, dm("and the takeaway?"))
	fc.take()
	if got := allUserContent(lastChatReq(fm)); !strings.Contains(got, "wasn't kept") || strings.Contains(got, "Quarterly revenue") {
		t.Fatalf("follow-up should say the text wasn't kept:\n%s", got)
	}
}
