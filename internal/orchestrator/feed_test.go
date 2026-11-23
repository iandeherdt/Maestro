package orchestrator

import (
	"fmt"
	"testing"
	"time"
)

func TestAddFeedKeepsTheLastLines(t *testing.T) {
	at := time.Date(2026, 11, 23, 20, 0, 0, 0, time.UTC)
	var feed []FeedItem
	feed = addFeed(feed, Event{Kind: "tool_result"}, at)
	feed = addFeed(feed, Event{Kind: "notification"}, at)
	feed = addFeed(feed, Event{Kind: "other_message", Message: "x"}, at)
	if len(feed) != 0 {
		t.Fatalf("empty or unimportant events kept: %v", feed)
	}
	for i := 0; i < FeedLen+5; i++ {
		feed = addFeed(feed, Event{Kind: "tool_call", Message: fmt.Sprint(i)}, at)
	}
	if len(feed) != FeedLen || feed[0].Message != "5" || feed[FeedLen-1].Message != fmt.Sprint(FeedLen+4) {
		t.Fatalf("feed %d lines, first %q, last %q", len(feed), feed[0].Message, feed[len(feed)-1].Message)
	}
}
