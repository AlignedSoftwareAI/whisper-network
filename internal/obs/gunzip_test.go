package obs

import (
	"compress/gzip"
	"io"
)

func gunzip(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }
