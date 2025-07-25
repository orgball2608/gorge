package payload

// CachePayload is a struct stored in L2 cache, containing both data and metadata.
type CachePayload[T any] struct {
	Data  T
	IsNil bool // Marks this as negative cache
}
