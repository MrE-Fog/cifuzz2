package fileutil

import (
	"io/fs"
	"io/ioutil"
	"os"

	"code-intelligence.com/cifuzz/pkg/log"
	"github.com/bazelbuild/rules_go/go/tools/bazel"
	"github.com/pkg/errors"
)

func IsSymlink(path string) bool {
	f, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return f.Mode()&os.ModeSymlink != 0
}

// IsDir returns whether this path is a directory. Tries to behave the
// same as Python's pathlib.Path.is_dir()
func IsDir(path string) bool {
	f, err := os.Stat(path)
	if err != nil {
		return false
	}
	return f.Mode()&os.ModeDir != 0
}

// Touch creates a file at the given path
func Touch(path string) error {
	file, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0644)
	if err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	err = file.Close()
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return false, errors.WithStack(err)
	}
	return !os.IsNotExist(err), nil
}

// TempFile creates a temporary file in the Bazel test temp dir
// if TEST_TMPDIR is defined, else in the OS default temp dir.
func TempFile(pattern string) (*os.File, error) {
	res, err := ioutil.TempFile(bazel.TestTmpDir(), pattern)
	return res, errors.WithStack(err)
}

// TempDir just calls bazel.NewTmpDir(). We provide it only for
// completeness, because TempFile is also provided by this package.
func TempDir(prefix string) (string, error) {
	res, err := bazel.NewTmpDir(prefix)
	return res, errors.WithStack(err)
}

// Cleanup removes the specified file or directory and prints any errors
// to stderr. It's supposed to be used in defer statements to clean up
// temporary directories.
func Cleanup(path string) {
	if os.Getenv("SKIP_CLEANUP") != "" {
		return
	}

	err := os.RemoveAll(path)
	if err != nil {
		log.Warnf("%+v", errors.WithStack(err))
	}
}

// CopyFile creates a copy of src at dest in a simple and not very
// efficient way.
func CopyFile(src, dest string, perm fs.FileMode) error {
	bytes, err := ioutil.ReadFile(src)
	if err != nil {
		return errors.WithStack(err)
	}
	err = ioutil.WriteFile(dest, bytes, perm)
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}