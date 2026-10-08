package main

import (
	"context"
	"time"
)

// prwarm.go starts what a PR review page asks for first -- the conversation
// and the comments already on the PR -- as soon as the review starts, in
// parallel with the checkout (checkoutPR). Neither needs the checkout, only
// the PR's address and the token, and each is a GitHub round trip the page
// would otherwise wait on after it loads. The first request for each takes
// the result, waiting for it if it is still on its way; later requests fetch
// fresh as before.

// prWarmMaxAge is how old a warmed result may be when the page first asks:
// the page normally loads seconds after the checkout. Older, it is fetched
// again.
const prWarmMaxAge = 30 * time.Second

// prWarmTimeout bounds each warm fetch, independent of the checkout's context.
const prWarmTimeout = 30 * time.Second

// prWarm is one fetch started early. done closes when val and err are set.
type prWarm[T any] struct {
	done chan struct{}
	val  T
	err  error
	at   time.Time
}

func startWarm[T any](fetch func(context.Context) (T, error)) *prWarm[T] {
	w := &prWarm[T]{done: make(chan struct{})}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), prWarmTimeout)
		defer cancel()
		w.val, w.err = fetch(ctx)
		w.at = time.Now()
		close(w.done)
	}()
	return w
}

// take waits for the result, up to ctx. ok is false when it failed, is too
// old, or ctx ended first: then the caller fetches as usual.
func (w *prWarm[T]) take(ctx context.Context) (val T, at time.Time, ok bool) {
	if w == nil {
		return val, at, false
	}
	select {
	case <-w.done:
	case <-ctx.Done():
		return val, at, false
	}
	if w.err != nil || time.Since(w.at) > prWarmMaxAge {
		return val, at, false
	}
	return w.val, w.at, true
}

type prCommentsResult struct {
	issue, review []PRComment
}

// prWarmSet is everything warmed for one review. Each field is used once:
// the handler that takes it clears it under prSession.mu.
type prWarmSet struct {
	conv     *prWarm[PRConversation] // nil without a token: the page then shows needsToken
	comments *prWarm[prCommentsResult]
}

func startPRWarm(provider GitProvider, target PRTarget, token string) *prWarmSet {
	ws := &prWarmSet{
		comments: startWarm(func(ctx context.Context) (prCommentsResult, error) {
			issue, review, err := provider.FetchComments(ctx, target, token)
			return prCommentsResult{issue, review}, err
		}),
	}
	if token != "" {
		ws.conv = startWarm(func(ctx context.Context) (PRConversation, error) {
			conv, err := provider.FetchConversation(ctx, target, token)
			if err == nil {
				renderConversationHTML(&conv)
			}
			return conv, err
		})
	}
	return ws
}

// takeWarmConv hands the warmed conversation to its first request.
func (p *prSession) takeWarmConv() *prWarm[PRConversation] {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warm == nil {
		return nil
	}
	w := p.warm.conv
	p.warm.conv = nil
	return w
}

// takeWarmComments hands the warmed comments to their first request.
func (p *prSession) takeWarmComments() *prWarm[prCommentsResult] {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warm == nil {
		return nil
	}
	w := p.warm.comments
	p.warm.comments = nil
	return w
}
