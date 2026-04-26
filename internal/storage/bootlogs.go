package storage

import "io"

// WriteBootLog persists a boot log shipped by the device's uploader. Boot logs
// have no route or segment -- the device walks /data/media/0/realdata/boot/
// and PUTs each file with path "boot/<id>.zst", so we store them flat at
// <basePath>/<dongleID>/boot/<id>.zst.
func (s *Storage) WriteBootLog(dongleID, id string, data io.Reader) error {
	return s.writeAuxiliaryFile(dongleID, "boot", id, ".zst", data)
}
