/*
Copyright 2015 Google Inc. All rights reserved.

Use of this source code is governed by a BSD-style
license that can be found in the LICENSE file or at
https://developers.google.com/open-source/licenses/bsd
*/

package cups

/*
#include "cups.h"
*/
import "C"
import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sync"
	"unsafe"

	"github.com/google/cups-connector/cdd"
)

// This isn't really a cache, but an interface to CUPS' quirky PPD interface.
// The connector needs to know when a PPD changes, but the CUPS API can only:
// (1) fetch a PPD to a file
// (2) indicate whether a PPD file is up-to-date.
// So, this "cache":
// (1) maintains temporary file copies of PPDs for each printer
// (2) updates those PPD files as necessary
type ppdCache struct {
	cc         *cupsCore
	cache      map[string]*ppdCacheEntry
	cacheMutex sync.RWMutex
}

func newPPDCache(cc *cupsCore) *ppdCache {
	cache := make(map[string]*ppdCacheEntry)
	pc := ppdCache{
		cc:    cc,
		cache: cache,
	}
	return &pc
}

func (pc *ppdCache) quit() {
	pc.cacheMutex.Lock()
	defer pc.cacheMutex.Unlock()

	for printername, pce := range pc.cache {
		pce.free()
		delete(pc.cache, printername)
	}
}

// removePPD removes a cache entry from the cache.
func (pc *ppdCache) removePPD(printername string) {
	pc.cacheMutex.Lock()
	defer pc.cacheMutex.Unlock()

	if pce, exists := pc.cache[printername]; exists {
		pce.free()
		delete(pc.cache, printername)
	}
}

func (pc *ppdCache) getPPDCacheEntry(printername string) (*cdd.PrinterDescriptionSection, string, string, error) {
	pc.cacheMutex.RLock()
	pce, exists := pc.cache[printername]
	pc.cacheMutex.RUnlock()

	if !exists {
		pce, err := createPPDCacheEntry(printername)
		if err != nil {
			return nil, "", "", err
		}
		if err = pce.refresh(pc.cc); err != nil {
			pce.free()
			return nil, "", "", err
		}

		pc.cacheMutex.Lock()
		defer pc.cacheMutex.Unlock()

		if firstPCE, exists := pc.cache[printername]; exists {
			// Two entries were created at the same time. Remove the older one.
			delete(pc.cache, printername)
			go firstPCE.free()
		}
		pc.cache[printername] = pce
		description, manufacturer, model := pce.getFields()
		return &description, manufacturer, model, nil

	} else {
		if err := pce.refresh(pc.cc); err != nil {
			delete(pc.cache, printername)
			pce.free()
			return nil, "", "", err
		}
		description, manufacturer, model := pce.getFields()
		return &description, manufacturer, model, nil
	}
}

// Holds persistent data needed for calling C.cupsGetPPD3.
type ppdCacheEntry struct {
	printername  *C.char
	modtime      C.time_t
	filename     string
	description  cdd.PrinterDescriptionSection
	manufacturer string
	model        string
	mutex        sync.Mutex
}

// createPPDCacheEntry creates an instance of ppdCache with the name field set,
// all else empty. The caller must free the name and buffer fields with
// ppdCacheEntry.free()
func createPPDCacheEntry(name string) (*ppdCacheEntry, error) {
	file, err := ioutil.TempFile("", "cups-connector-ppd-")
	if err != nil {
		return nil, fmt.Errorf("Failed to create PPD cache entry file: %s", err)
	}
	defer file.Close()
	filename := file.Name()

	pce := &ppdCacheEntry{
		printername: C.CString(name),
		modtime:     C.time_t(0),
		filename:    filename,
	}

	return pce, nil
}

// getFields gets externally-interesting fields from this ppdCacheEntry under
// a lock. The description is passed as a value (copy), to protect the cached copy.
func (pce *ppdCacheEntry) getFields() (cdd.PrinterDescriptionSection, string, string) {
	pce.mutex.Lock()
	defer pce.mutex.Unlock()
	return pce.description, pce.manufacturer, pce.model
}

// free frees the memory that stores the name and buffer fields, and deletes
// the file named by the buffer field. If the file doesn't exist, no error is
// returned.
func (pce *ppdCacheEntry) free() {
	pce.mutex.Lock()
	defer pce.mutex.Unlock()

	C.free(unsafe.Pointer(pce.printername))
	os.Remove(pce.filename)
}

// refresh calls cupsGetPPD3() to refresh this PPD information, in
// case CUPS has a new PPD for the printer.
func (pce *ppdCacheEntry) refresh(cc *cupsCore) error {
	pce.mutex.Lock()
	defer pce.mutex.Unlock()

	ppdFilename, err := cc.getPPD(pce.printername, &pce.modtime)
	if err != nil {
		return err
	}

	if ppdFilename == nil {
		// Cache hit.
		return nil
	}

	// (else) Cache miss.
	defer C.free(unsafe.Pointer(ppdFilename))
	defer os.Remove(C.GoString(ppdFilename))

	// Read from CUPS temporary file.
	r, err := os.Open(C.GoString(ppdFilename))
	if err != nil {
		return err
	}
	defer r.Close()

	// Write to this PPD cache file.
	file, err := os.OpenFile(pce.filename, os.O_WRONLY|os.O_TRUNC, 0200)
	if err != nil {
		return fmt.Errorf("Failed to open already-created PPD cache file: %s", err)
	}
	defer file.Close()

	// Also write to a buffer for translation.
	var content bytes.Buffer

	// Write to these simultaneously.
	w := io.MultiWriter(&content, file)
	if _, err := io.Copy(w, r); err != nil {
		return err
	}

	description, manufacturer, model := translatePPD(content.String())
	if description == nil || manufacturer == "" || model == "" {
		return errors.New("Failed to parse PPD")
	}

	pce.description = *description
	pce.manufacturer = manufacturer
	pce.model = model

	return nil
}
