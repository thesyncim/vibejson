package legacy

import stdlibcorpus "github.com/thesyncim/simdjson/tests/stdlib"

// Keep the legacy benchmark's local names while sourcing every model from the
// canonical Go standard-library corpus package.
type (
	canadaRoot  = stdlibcorpus.CanadaRoot
	citmRoot    = stdlibcorpus.CITMRoot
	golangRoot  = stdlibcorpus.GolangRoot
	stringRoot  = stdlibcorpus.StringRoot
	syntheaRoot = stdlibcorpus.SyntheaRoot
	twitterRoot = stdlibcorpus.TwitterRoot
)
