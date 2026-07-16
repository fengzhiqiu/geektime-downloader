package progress

// Reporter receives download progress events. All methods are optional no-ops when nil.
type Reporter interface {
	OnArticleStart(aid int, title, phase string)
	OnArticleComplete(aid int, files []string)
	OnArticleSkipped(aid int, reason string)
	OnArticleFailed(aid int, err error)
	OnArticleProgress(aid, done, total int)
}

// Nop ignores all progress events.
type Nop struct{}

func (Nop) OnArticleStart(int, string, string) {}
func (Nop) OnArticleComplete(int, []string)   {}
func (Nop) OnArticleSkipped(int, string)        {}
func (Nop) OnArticleFailed(int, error)          {}
func (Nop) OnArticleProgress(int, int, int)     {}
