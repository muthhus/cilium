// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2021 Authors of Cilium

package elf

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"unsafe"

	"github.com/sirupsen/logrus"

	"github.com/cilium/cilium/pkg/lock"
)

var (
	ignoredPrefixes []string
)

// ELF is an in-memory representation of a BPF ELF object from the filesystem.
type ELF struct {
	metadata *elf.File
	file     *os.File
	symbols  symbols
	log      *logrus.Entry

	// lock concurrent writes of the ELF. This library probably isn't far
	// off from allowing concurrent Write() execution, but it's just not
	// supported for now.
	lock.Mutex
}

// newReader creates a new reader that can safely seek the input in parallel.
func newReader(ra io.ReaderAt) *io.SectionReader {
	// If 1<<63-1 is good enough for pkg/debug/elf, it's good enough for us.
	return io.NewSectionReader(ra, 0, 1<<63-1)
}

// NewELF returns a new object from the specified reader.
//
// The ELF binary is expected to start at position 0 in the specified reader.
func NewELF(ra io.ReaderAt, scopedLog *logrus.Entry) (*ELF, error) {
	ef, err := elf.NewFile(ra)
	if err != nil {
		return nil, fmt.Errorf("unable to open ELF: %s", err)
	}

	// EM_NONE is generated by older Clang (eg 3.8.x), which we currently
	// use in Travis. We should be able to drop that part pretty soon.
	if ef.Machine != elf.EM_NONE && ef.Machine != elf.EM_BPF {
		return nil, fmt.Errorf("unsupported ELF machine type %s", ef.Machine)
	}

	result := &ELF{
		metadata: ef,
		log:      scopedLog,
	}
	if err := result.symbols.extractFrom(ef); err != nil {
		return nil, fmt.Errorf("unable to read ELF symbols: %s", err)
	}

	return result, nil
}

// Open an ELF file from the specified path.
func Open(path string) (*ELF, error) {
	scopedLog := log.WithField(srcPath, path)

	f, err := os.Open(path)
	if err != nil {
		return nil, &os.PathError{
			Op:   "failed to open ELF file",
			Path: path,
			Err:  err,
		}
	}

	result, err := NewELF(f, scopedLog)
	if err != nil {
		if err2 := f.Close(); err2 != nil {
			scopedLog.WithError(err).Warning("Failed to close ELF")
		}
		return nil, &os.PathError{
			Op:   "failed to parse ELF file",
			Path: path,
			Err:  err,
		}
	}
	result.file = f
	return result, nil
}

// Close closes the ELF. If the File was created using NewELF directly instead
// of Open, Close has no effect.
func (elf *ELF) Close() (err error) {
	if elf.file != nil {
		err = elf.file.Close()
	}
	return err
}

func (elf *ELF) writeValue(w io.WriteSeeker, offset uint64, value []byte) error {
	if _, err := w.Seek(int64(offset), io.SeekStart); err != nil {
		return err
	}
	return binary.Write(w, elf.metadata.ByteOrder, value)
}

// copy the ELF from the reader to the writer, substituting the constants with
// names specified in 'intOptions' with their corresponding values, and the
// strings specified in 'strOptions' with their corresponding values.
//
// Keys in the 'intOptions' / 'strOptions' maps are case-sensitive.
func (elf *ELF) copy(w io.WriteSeeker, r *io.SectionReader, intOptions map[string]uint32, strOptions map[string]string) error {
	if len(intOptions) == 0 && len(strOptions) == 0 {
		// Copy the remaining portion of the file
		if _, err := io.Copy(w, r); err != nil {
			return err
		}
		return nil
	}

	// Copy the ELF's contents, we overwrite at specific offsets later.
	if _, err := io.Copy(w, r); err != nil {
		return err
	}

	processedOptions := make(map[string]struct{}, len(intOptions)+len(strOptions))

processSymbols:
	for _, symbol := range elf.symbols.sort() {
		scopedLog := log.WithField("symbol", symbol.name)

		// Figure out the value to substitute
		var value []byte
		switch symbol.kind {
		case symbolData:
			if v, exists := intOptions[symbol.name]; exists {
				value = make([]byte, symbol.size)
				switch uintptr(symbol.size) {
				case unsafe.Sizeof(uint32(0)):
					elf.metadata.ByteOrder.PutUint32(value, v)
				case unsafe.Sizeof(uint16(0)):
					elf.metadata.ByteOrder.PutUint16(value, uint16(v))
				}
			}

		case symbolString:
			v, exists := strOptions[symbol.name]
			if exists {
				if uint64(len(v)) != symbol.size {
					return fmt.Errorf("symbol substitution value %q (len %d) must equal length of symbol name %q (len %d)", v, len(v), symbol.name, symbol.size)
				}
				value = []byte(v)
			}
		}

		if value == nil {
			for _, prefix := range ignoredPrefixes {
				if strings.HasPrefix(symbol.name, prefix) {
					continue processSymbols
				}
			}
			scopedLog.Warning("Skipping symbol substitution")
			continue processSymbols
		}

		// Encode the value at the given offset in the destination file.
		if err := elf.writeValue(w, symbol.offset, value); err != nil {
			return fmt.Errorf("failed to substitute %s: %s", symbol.name, err)
		}

		if symbol.offsetBTF != 0 {
			if err := elf.writeValue(w, symbol.offsetBTF, value); err != nil {
				return fmt.Errorf("failed to substitute %s BTF: %s", symbol.name, err)
			}
		}

		processedOptions[symbol.name] = struct{}{}
	}

	// Check for additional options that weren't applied
	for symbol := range strOptions {
		if _, processed := processedOptions[symbol]; !processed {
			return fmt.Errorf("no such string %q in ELF", symbol)
		}
	}
	for symbol := range intOptions {
		if _, processed := processedOptions[symbol]; !processed {
			return fmt.Errorf("no such symbol %q in ELF", symbol)
		}
	}

	return nil
}

// Write the received ELF to a new file at the specified location, with the
// specified options (indexed by name) substituted:
// - intOptions: 32-bit values substituted in the data section.
// - strOptions: strings susbtituted in the string table. For each key/value
//               pair, both key and value must be same length.
//
// Only one goroutine may Write() the same *ELF concurrently.
//
// On success, writes the new file to the specified path.
// On failure, returns an error and no file is left on the filesystem.
func (elf *ELF) Write(path string, intOptions map[string]uint32, strOptions map[string]string) error {
	elf.Lock()
	defer elf.Unlock()

	scopedLog := elf.log.WithField(dstPath, path)

	f, err := os.Create(path)
	if err != nil {
		return &os.PathError{
			Op:   "failed to create ELF file",
			Path: path,
			Err:  err,
		}
	}

	defer func() {
		if err2 := f.Close(); err2 != nil {
			scopedLog.WithError(err).Warning("Failed to close new ELF")
		}
		if err != nil {
			if err2 := os.RemoveAll(path); err2 != nil {
				scopedLog.WithError(err).Warning("Failed to clean up new ELF path on error")
			}
		}
	}()

	reader := newReader(elf.file)
	if err = elf.copy(f, reader, intOptions, strOptions); err != nil {
		return &os.PathError{
			Op:   "failed to write ELF file:",
			Path: path,
			Err:  err,
		}
	}
	if err = f.Sync(); err != nil {
		return &os.PathError{
			Op:   "failed to sync ELF file:",
			Path: path,
			Err:  err,
		}
	}

	scopedLog.WithError(err).Debugf("Finished writing ELF")
	return nil
}

// IgnoreSymbolPrefixes configures the ELF package to ignore symbols that have
// any of the specified prefixes. It must be called by only one thread at a time.
//
// This slice will be iterated once per ELF.Write(), so try not to let it grow
// out of hand...
func IgnoreSymbolPrefixes(prefixes []string) {
	ignoredPrefixes = prefixes
}
