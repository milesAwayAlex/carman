package memory

import (
	"math"
	"strings"
	"unicode"

	"github.com/kljensen/snowball/english"
)

// Search works on word stems: text is lower-cased, split into words, common
// words are dropped and the rest are stemmed, so "deploying" and "deployed"
// both become "deploy".

var stopwords = func() map[string]bool {
	words := strings.Fields(`a about above after again against all am an and any are as at be because been
		before being below between both but by can did do does doing don down during each few for from further
		had has have having he her here hers herself him himself his how i if in into is it its itself just me
		more most my myself no nor not now of off on once only or other our ours ourselves out over own s same
		she should so some such t than that the their theirs them themselves then there these they this those
		through to too under until up very was we were what when where which while who whom why will with you
		your yours yourself yourselves`)
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}()

// splitCompound puts a space inside words such as topologySpreadConstraints,
// so that a search for the identifier finds a note written in plain words.
func splitCompound(text string) string {
	var b strings.Builder
	var previous rune
	for _, r := range text {
		if unicode.IsUpper(r) && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
		previous = r
	}
	return b.String()
}

// stems returns how often each word stem occurs in the text.
func stems(text string) map[string]int {
	counts := map[string]int{}
	words := strings.FieldsFunc(strings.ToLower(splitCompound(text)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, word := range words {
		if stopwords[word] {
			continue
		}
		counts[english.Stem(word, false)]++
	}
	return counts
}

// document is what search knows about one entry.
type document struct {
	stems  map[string]int
	length int            // number of stemmed words
	head   map[string]int // the stems of the entry's opening; see headLength
	title  map[string]int // the stems of its first paragraph, which says what it is
}

// headLength is how much of an entry counts when judging whether it is about
// the same thing as some other text (overlap). An entry is usually a sentence
// or two, and all of it counts. But an entry can also be a long working
// document, and a long document contains most common words: measured whole,
// it would look close to every prompt and to every new note. What such an
// entry is about is said in its opening lines.
const headLength = 600

func newDocument(text string) document {
	d := document{stems: stems(text)}
	for _, n := range d.stems {
		d.length += n
	}
	d.head = d.stems
	if runes := []rune(text); len(runes) > headLength {
		d.head = stems(string(runes[:headLength]))
	}
	d.title = d.head
	if first, _, cut := strings.Cut(strings.TrimSpace(text), "\n\n"); cut {
		if runes := []rune(first); len(runes) > headLength {
			first = string(runes[:headLength])
		}
		d.title = stems(first)
	}
	return d
}

// corpus holds the statistics that ranking needs across all entries.
type corpus struct {
	documents map[string]document
	frequency map[string]int // how many documents contain each stem
	length    int            // total length of all documents
}

func newCorpus() *corpus {
	return &corpus{documents: map[string]document{}, frequency: map[string]int{}}
}

func (c *corpus) add(id, text string) {
	d := newDocument(text)
	c.documents[id] = d
	c.length += d.length
	for stem := range d.stems {
		c.frequency[stem]++
	}
}

// score ranks a document against a query with BM25: a document scores higher
// for each query stem it contains, more so when that stem is rare across the
// library, and less so when the document is long.
func (c *corpus) score(id string, query map[string]int) float64 {
	const k1, b = 1.2, 0.75
	d := c.documents[id]
	n := float64(len(c.documents))
	average := float64(c.length) / math.Max(n, 1)
	var total float64
	for stem := range query {
		tf := float64(d.stems[stem])
		if tf == 0 {
			continue
		}
		df := float64(c.frequency[stem])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		total += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(d.length)/math.Max(average, 1)))
	}
	return total
}

// rank is score with two additions for search: a stem found in the entry's
// first paragraph counts again, weighted by how rare it is, and the number of
// distinct query stems the entry contains is reported. The first paragraph
// says what an entry is; a long body mentions many things in passing, and
// without this an unrelated document outranked the one that was asked for.
func (c *corpus) rank(id string, query map[string]int) (score float64, matched int) {
	score = c.score(id, query)
	d := c.documents[id]
	n := float64(len(c.documents))
	for stem := range query {
		if d.stems[stem] > 0 {
			matched++
		}
		if d.title[stem] > 0 {
			df := float64(c.frequency[stem])
			score += 1.5 * math.Log(1+(n-df+0.5)/(df+0.5))
		}
	}
	return score, matched
}

// overlap measures how closely a document and a query are about the same
// thing, for deciding whether to volunteer the document unasked. matched is
// the number of distinct stems they share. share is the larger of two
// fractions: how much of the document the query covers, and how much of the
// query the document covers. A long error message can then bring up a short
// entry it fully contains, and a short prompt an entry that contains all of it.
func (c *corpus) overlap(id string, query map[string]int) (matched int, share float64) {
	d := c.documents[id]
	if len(d.head) == 0 || len(query) == 0 {
		return 0, 0
	}
	for stem := range query {
		if d.head[stem] > 0 {
			matched++
		}
	}
	return matched, math.Max(float64(matched)/float64(len(d.head)), float64(matched)/float64(len(query)))
}
