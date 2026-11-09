package messages

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
)

// warmSigner is the proxy with its warm-up: every image is 320x200, and nothing is fetched
// until the test lets it (so the request has returned before the background work starts).
type warmSigner struct {
	fakeSigner
	release chan struct{}
	mu      sync.Mutex
	warmed  []string
}

func (w *warmSigner) Warm(_ context.Context, src string) (int, int, bool) {
	<-w.release
	w.mu.Lock()
	w.warmed = append(w.warmed, src)
	w.mu.Unlock()
	return 320, 200, true
}

func (w *warmSigner) fetched() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]string(nil), w.warmed...)
	sort.Strings(out)
	return out
}

// sizedMessages records the sizes the warm-up writes back, as the repository's compare-and-set.
type sizedMessages struct {
	embedMessages
	replaced chan bool
}

func (f sizedMessages) ReplaceEmbeds(_ context.Context, _, msgID int64, from, to string) (bool, error) {
	m := f.stored[msgID]
	ok := m != nil && m.EmbedsJSON == from
	if ok {
		cp := *m
		cp.EmbedsJSON = to
		f.stored[msgID] = &cp
	}
	f.replaced <- ok
	return ok, nil
}

func newWarmWorld(t *testing.T) (*embedWorld, *warmSigner, sizedMessages) {
	w := newEmbedWorld()
	sig := &warmSigner{release: make(chan struct{})}
	repo := sizedMessages{embedMessages{w.repo}, make(chan bool, 1)}
	w.svc.repo = repo
	w.svc.SetMediaSigner(sig)
	t.Cleanup(func() {
		select {
		case <-sig.release:
		default:
			close(sig.release)
		}
	})
	return w, sig, repo
}

func waitReplaced(t *testing.T, repo sizedMessages) bool {
	t.Helper()
	select {
	case ok := <-repo.replaced:
		return ok
	case <-time.After(5 * time.Second):
		t.Fatal("the sizes were never written back")
		return false
	}
}

func TestEmbedImagesAreWarmedAtPostTime(t *testing.T) {
	w, sig, repo := newWarmWorld(t)
	ctx := context.Background()
	m, err := w.svc.Create(ctx, embedBot, mirrorRoom, &CreateMessageInput{
		Attachments: []string{id.Format(chartID)},
		Embeds: embedsIn(
			Embed{
				Title:     "t",
				Image:     &EmbedImage{URL: "https://img.example/big.png"},
				Thumbnail: &EmbedImage{URL: "attachment://chart.png"},
				Author:    &EmbedAuthor{Name: "a", IconURL: "https://img.example/a.png"},
			},
			// The same picture twice is one fetch.
			Embed{Title: "u", Thumbnail: &EmbedImage{URL: "https://img.example/big.png"}},
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	close(sig.release)
	if !waitReplaced(t, repo) {
		t.Fatal("the write-back did not apply")
	}
	// Only what is not on this instance's CDN goes through the proxy.
	if got := sig.fetched(); len(got) != 2 || got[0] != "https://img.example/a.png" || got[1] != "https://img.example/big.png" {
		t.Fatalf("warmed %v", got)
	}
	list := w.repo.stored[m.ID].Embeds()
	if img := list[0].Image; img.Width != 320 || img.Height != 200 {
		t.Fatalf("image %+v", img)
	}
	if th := list[0].Thumbnail; th.URL != chartURL || th.Width != 640 || th.Height != 480 {
		t.Fatalf("the upload's own size was replaced: %+v", th)
	}
	if th := list[1].Thumbnail; th.Width != 320 {
		t.Fatalf("second embed %+v", th)
	}

	// An edit that sets embeds warms them; one that only changes the text does not.
	sig.release = make(chan struct{})
	sig.warmed = nil
	if _, err := w.svc.Edit(ctx, embedBot, mirrorRoom, m.ID, &EditMessageInput{Plaintext: "just text"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Edit(ctx, embedBot, mirrorRoom, m.ID, &EditMessageInput{
		Embeds: embedsIn(Embed{Title: "v", Image: &EmbedImage{URL: "https://img.example/new.png"}}),
	}); err != nil {
		t.Fatal(err)
	}
	close(sig.release)
	waitReplaced(t, repo)
	if got := sig.fetched(); len(got) != 1 || got[0] != "https://img.example/new.png" {
		t.Fatalf("an edit warmed %v", got)
	}
	if img := w.repo.stored[m.ID].Embeds()[0].Image; img.Width != 320 {
		t.Fatalf("edited image %+v", img)
	}
}

func TestRelayedEmbedsAreWarmedHere(t *testing.T) {
	w, sig, repo := newWarmWorld(t)
	m := &Message{ID: 902, RoomID: mirrorRoom, SenderID: carol, Plaintext: "hi", CreatedAt: time.Now()}
	m.SetEmbeds([]Embed{{Title: "t", Image: &EmbedImage{URL: "https://img.example/r.png"}}})
	if err := w.svc.CreateFederated(context.Background(), mirrorRoom, nil, m); err != nil {
		t.Fatal(err)
	}
	close(sig.release)
	waitReplaced(t, repo)
	if img := w.repo.stored[902].Embeds()[0].Image; img.Width != 320 || len(sig.fetched()) != 1 {
		t.Fatalf("relayed image %+v, warmed %v", img, sig.fetched())
	}
}

func TestWarmingIsBounded(t *testing.T) {
	w, sig, _ := newWarmWorld(t)
	for i := 0; i < cap(warmQueue); i++ {
		warmQueue <- struct{}{}
	}
	defer func() {
		for len(warmQueue) > 0 {
			<-warmQueue
		}
	}()
	m := &Message{ID: 903, RoomID: mirrorRoom, Plaintext: "hi"}
	m.SetEmbeds([]Embed{{Title: "t", Image: &EmbedImage{URL: "https://img.example/q.png"}}})
	w.svc.warmEmbeds(m)
	if len(warmQueue) != cap(warmQueue) {
		t.Fatal("a warm-up started past the bound")
	}
	close(sig.release)
	if got := sig.fetched(); len(got) != 0 {
		t.Fatalf("warmed %v", got)
	}
}
