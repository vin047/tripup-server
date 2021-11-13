package storage

type StorageClient interface {
    Filesizes(string) (uint64, uint64, error)
    Delete(paths []string) error
}
