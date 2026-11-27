package worker

import "os"

func (r *Resident) syncUploaded(file *os.File) error {
	if r.options.syncUploadedFile != nil {
		return r.options.syncUploadedFile(file)
	}
	return file.Sync()
}
