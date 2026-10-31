package raise

import "context"

// Page is one cursor page. NextCursor is nil when the list ends.
type Page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

// Iter walks every item of a cursor list.
type Iter[T any] struct {
	fetch  func(ctx context.Context, cursor *string) (*Page[T], error)
	items  []T
	idx    int
	cursor *string
	err    error
	loaded bool
	done   bool
}

func newIter[T any](fetch func(ctx context.Context, cursor *string) (*Page[T], error)) *Iter[T] {
	return &Iter[T]{fetch: fetch, idx: -1}
}

// Next loads the next item, fetching a page when the current one is exhausted.
func (it *Iter[T]) Next(ctx context.Context) bool {
	if it == nil || it.err != nil || it.done {
		return false
	}
	if it.idx+1 < len(it.items) {
		it.idx++
		return true
	}
	if it.loaded && (it.cursor == nil || *it.cursor == "") {
		it.done = true
		return false
	}
	page, err := it.fetch(ctx, it.cursor)
	if err != nil {
		it.err = err
		return false
	}
	it.loaded = true
	if page == nil {
		it.done = true
		return false
	}
	it.items = page.Data
	it.cursor = page.NextCursor
	it.idx = 0
	if len(it.items) == 0 {
		it.done = true
		return false
	}
	return true
}

// Item is the item Next last yielded. Call only after Next returned true.
func (it *Iter[T]) Item() T {
	return it.items[it.idx]
}

// Err is the first fetch error, if any.
func (it *Iter[T]) Err() error {
	if it == nil {
		return nil
	}
	return it.err
}
