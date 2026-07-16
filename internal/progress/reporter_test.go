package progress

import "testing"

type captureReporter struct {
	progresses []progressEvent
}
type progressEvent struct{ aid, done, total int }

func (c *captureReporter) OnArticleStart(int, string, string)      {}
func (c *captureReporter) OnArticleComplete(int, []string)        {}
func (c *captureReporter) OnArticleSkipped(int, string)           {}
func (c *captureReporter) OnArticleFailed(int, error)            {}
func (c *captureReporter) OnArticleProgress(aid, done, total int) {
	c.progresses = append(c.progresses, progressEvent{aid, done, total})
}

func TestNopOnArticleProgress(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Nop.OnArticleProgress must not panic: %v", r)
		}
	}()
	var n Nop
	n.OnArticleProgress(1, 2, 10)
}
