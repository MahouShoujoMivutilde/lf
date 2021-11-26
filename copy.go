package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"log"
)

func copySize(srcs []string) (int64, error) {
	var total int64

	for _, src := range srcs {
		_, err := os.Lstat(src)
		if os.IsNotExist(err) {
			return total, fmt.Errorf("src does not exist: %q", src)
		}

		err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return fmt.Errorf("walk: %s", err)
			}
			total += info.Size()
			return nil
		})

		if err != nil {
			return total, err
		}
	}

	return total, nil
}

// This is a piece of code from `io.copyBuffer()` responsible for a long chain of
// actions leading to reflink copy
func iocopyKnockoff(dst io.Writer, src io.Reader) (written int64, err error) {
	// If the reader has a WriteTo method, use it to do the copy.
	// Avoids an allocation and a copy.
	if wt, ok := src.(io.WriterTo); ok {
		log.Printf("Picked WriterTo")
		return wt.WriteTo(dst)
	}
	// Similarly, if the writer has a ReadFrom method, use it to do the copy.
	// FIXME it always picks this, even when copy is between different file systems
	if rt, ok := dst.(io.ReaderFrom); ok {
		log.Printf("Picked ReaderFrom")
		return rt.ReadFrom(src)
	}

	// No support for copy-on-write is not an error, falling back to normal copy
	log.Printf("Picked original loop copy")
	return -1, nil
}

func copyFile(src, dst string, info os.FileInfo, nums chan int64) error {
	buf := make([]byte, 4096)

	r, err := os.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()

	w, err := os.Create(dst)
	if err != nil {
		return err
	}

	// Right now this is equivalent to permanent `set reflink auto`
	//
	// Ideally this should be `io.CopyBuffer()` with custom buffer that tracks
	// progress (SOMEHOW*) when `set reflink auto` and it can't reflink;
	//
	// The buffer should be forced with `io.CopyBuffer(struct{ io.Writer }{w}, r, buf)` when `set reflink never`,
	// like here https://go.dev/doc/go1.15#os
	//
	// * - ... but I have no idea how to create something like that, hence why I opted for crude `iocopyKnockoff()`

	written, err := iocopyKnockoff(w, r)
	if err != nil {
		w.Close()
		os.Remove(dst)
		return err
	}

	// this never runs because ReaderFrom is always picked
	if written == -1 {
		for {
			n, err := r.Read(buf)
			if err != nil && err != io.EOF {
				w.Close()
				os.Remove(dst)
				return err
			}

			if n == 0 {
				break
			}

			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}

			nums <- int64(n)
		}
	} else {
		nums <- written
	}

	if err := w.Close(); err != nil {
		os.Remove(dst)
		return err
	}

	if err := os.Chmod(dst, info.Mode()); err != nil {
		os.Remove(dst)
		return err
	}

	return nil
}

func copyAll(srcs []string, dstDir string) (nums chan int64, errs chan error) {
	nums = make(chan int64, 1024)
	errs = make(chan error, 1024)

	go func() {
		for _, src := range srcs {
			dst := filepath.Join(dstDir, filepath.Base(src))

			_, err := os.Lstat(dst)
			if !os.IsNotExist(err) {
				var newPath string
				for i := 1; !os.IsNotExist(err); i++ {
					newPath = fmt.Sprintf("%s.~%d~", dst, i)
					_, err = os.Lstat(newPath)
				}
				dst = newPath
			}

			filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					errs <- fmt.Errorf("walk: %s", err)
					return nil
				}
				rel, err := filepath.Rel(src, path)
				if err != nil {
					errs <- fmt.Errorf("relative: %s", err)
					return nil
				}
				newPath := filepath.Join(dst, rel)
				if info.IsDir() {
					if err := os.MkdirAll(newPath, info.Mode()); err != nil {
						errs <- fmt.Errorf("mkdir: %s", err)
					}
					nums <- info.Size()
				} else if info.Mode()&os.ModeSymlink != 0 { /* Symlink */
					if rlink, err := os.Readlink(path); err != nil {
						errs <- fmt.Errorf("symlink: %s", err)
					} else {
						if err := os.Symlink(rlink, newPath); err != nil {
							errs <- fmt.Errorf("symlink: %s", err)
						}
					}
					nums <- info.Size()
				} else {
					if err := copyFile(path, newPath, info, nums); err != nil {
						errs <- fmt.Errorf("copy: %s", err)
					}
				}
				return nil
			})
		}

		close(errs)
	}()

	return nums, errs
}
