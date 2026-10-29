package web

import (
	"github.com/kenmoini/mcp-machine/internal/catalog"
)

// catalogPageSize is how many catalog cards the Add server page shows at once.
const catalogPageSize = 6

// pickData is what the Add server page renders: the catalog page requested.
type pickData struct {
	Entries     []catalog.Entry
	Page, Pages int // 1-based; Pages is 0 without a catalog
	Prev, Next  int // 0 when there is no previous or next page
	Total       int
}

// paginate returns page (1-based, clamped into range) of entries, size per
// page. No entries yields a zero pickData, which the template reads as "no
// catalog".
func paginate(entries []catalog.Entry, page, size int) pickData {
	n := len(entries)
	if n == 0 || size <= 0 {
		return pickData{}
	}
	pages := (n + size - 1) / size
	page = max(1, min(page, pages))
	start := (page - 1) * size
	d := pickData{Entries: entries[start:min(start+size, n)], Page: page, Pages: pages, Total: n}
	if page > 1 {
		d.Prev = page - 1
	}
	if page < pages {
		d.Next = page + 1
	}
	return d
}

// catalogEntries returns the catalog, or nil without one.
func (u *ui) catalogEntries() []catalog.Entry {
	if u.Catalog == nil {
		return nil
	}
	return u.Catalog()
}

// catalogEntry finds the catalog entry named name.
func (u *ui) catalogEntry(name string) (catalog.Entry, bool) {
	for _, e := range u.catalogEntries() {
		if e.Name == name {
			return e, true
		}
	}
	return catalog.Entry{}, false
}
