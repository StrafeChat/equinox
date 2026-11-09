package messages

import (
	"context"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/safego"
)

// MediaWarmer is the image proxy's other half, beyond signing: fetching an image before
// anyone views it, and learning its size on the way. The proxy (internal/modules/media)
// offers both; a signer without it simply leaves images to be fetched at first view.
type MediaWarmer interface {
	Warm(ctx context.Context, src string) (width, height int, ok bool)
}

const (
	// warmTimeout bounds one message's warm-up (a message has at most 10 embeds of at most 4
	// images each).
	warmTimeout = time.Minute
	// warmMessages is how many messages are warmed at once in this process - past it a
	// message's images are left to be fetched at first view - and warmFetches how many
	// images all of those warm-ups fetch at once.
	warmMessages = 64
	warmFetches  = 8
)

var (
	warmQueue = make(chan struct{}, warmMessages)
	warmSlots = make(chan struct{}, warmFetches)
)

// warmEmbeds fetches the external images of a message just written - created or edited, here
// or relayed from another instance - through the proxy, in the background. The site behind a
// picture then sees one fetch from this instance at post time, not one per reader (which
// would tell it when each of them read the message). Bounded: with warmMessages already
// being warmed, a message's images wait for their first view instead. The sizes learned go
// into the stored embeds so clients can reserve the space; no event is sent for that, a
// client simply gets them the next time it loads the message.
func (s *Service) warmEmbeds(m *Message) {
	warmer, ok := s.media.(MediaWarmer)
	if !ok || m == nil || m.Ciphertext != "" || m.EmbedsJSON == "" {
		return
	}
	list := m.Embeds()
	srcs := s.externalImages(list)
	if len(srcs) == 0 {
		return
	}
	select {
	case warmQueue <- struct{}{}:
	default:
		return
	}
	roomID, msgID, before := m.RoomID, m.ID, m.EmbedsJSON
	safego.Go("messages", func() {
		defer func() { <-warmQueue }()
		ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
		defer cancel()
		sizes := warmAll(ctx, warmer, srcs)
		if !withSizes(list, sizes) {
			return
		}
		after := encodeEmbeds(list)
		if after == "" || after == before {
			return
		}
		// Only over what was warmed: an edit that landed meanwhile wins.
		if _, err := s.repo.ReplaceEmbeds(ctx, roomID, msgID, before, after); err != nil {
			logger.Err("messages", err, map[string]any{"stage": "embed_sizes", "room_id": roomID})
		}
	})
}

// externalImages is every distinct picture in the embeds that is not already this instance's
// own (its CDN), in order.
func (s *Service) externalImages(list []Embed) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		if u == "" || seen[u] || s.ownsAttachment(u) || (s.media != nil && s.media.IsLocal(u)) {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, e := range list {
		if e.Image != nil {
			add(e.Image.URL)
		}
		if e.Thumbnail != nil {
			add(e.Thumbnail.URL)
		}
		if e.Author != nil {
			add(e.Author.IconURL)
		}
		if e.Footer != nil {
			add(e.Footer.IconURL)
		}
	}
	return out
}

// warmAll warms srcs, sharing the process's warmFetches slots with every other warm-up, and
// returns the sizes it learned. What is still waiting for a slot when ctx ends is skipped.
func warmAll(ctx context.Context, warmer MediaWarmer, srcs []string) map[string][2]int {
	out := make(map[string][2]int, len(srcs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, src := range srcs {
		select {
		case warmSlots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return out
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-warmSlots }()
			safego.Run("messages", func() {
				if w, h, ok := warmer.Warm(ctx, src); ok && w > 0 && h > 0 {
					mu.Lock()
					out[src] = [2]int{w, h}
					mu.Unlock()
				}
			})
		}()
	}
	wg.Wait()
	return out
}

// withSizes fills in the size of each image and thumbnail that has none, from sizes, and
// says whether anything changed.
func withSizes(list []Embed, sizes map[string][2]int) bool {
	changed := false
	fill := func(img *EmbedImage) {
		if img == nil || (img.Width > 0 && img.Height > 0) {
			return
		}
		if wh, ok := sizes[img.URL]; ok && learnedSizeOK(wh[0], wh[1]) {
			img.Width, img.Height = wh[0], wh[1]
			changed = true
		}
	}
	for i := range list {
		fill(list[i].Image)
		fill(list[i].Thumbnail)
	}
	return changed
}
