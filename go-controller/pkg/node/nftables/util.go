// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package nftables

import (
	"context"
	"fmt"

	"sigs.k8s.io/knftables"
)

// AddObjects adds each element of objects to nftables in a single transaction, using
// tx.Add() for each object.
func AddObjects(objects []knftables.Object) error {
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()
	for _, obj := range objects {
		tx.Add(obj)
	}
	return nft.Run(context.TODO(), tx)
}

// DeleteObjects deletes each element of objects from nftables, if it exists; no errors
// are returned for objects that don't exist.
//
// To avoid depending on `nft destroy` (which requires kernel 6.3+), this Add()s each
// element before Delete()ing it, so you won't get an error if the element wasn't already
// in the set/map, which means that for map elements, the Value field must be set to the
// correct value. Alternatively, you can leave Value unset, in which case
// DeleteNFTElements will first "list" the map to find its current contents, and then only
// try to delete the elements that are actually present.
func DeleteObjects(objects []knftables.Object) error {
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	// If there are any partial Map Elements, list their maps' existing contents
	existingMaps := make(map[string][]*knftables.Element)
	for _, obj := range objects {
		switch typed := obj.(type) {
		case *knftables.Element:
			if typed.Map != "" && len(typed.Value) == 0 && existingMaps[typed.Map] == nil {
				existingElements, err := nft.ListElements(context.TODO(), "map", typed.Map)
				if err != nil {
					return err
				}
				existingMaps[typed.Map] = existingElements
			}
		default:
			return fmt.Errorf("unsupported object type %T passed to DeleteObjects", obj)
		}
	}

	// Now build the actual transaction
	tx := nft.NewTransaction()
	for _, obj := range objects {
		switch typed := obj.(type) {
		case *knftables.Element:
			if typed.Map != "" && len(typed.Value) == 0 {
				// We can't tx.Add() a Map Element with no Value, so try
				// to find its existing value in the List output from
				// above. If the element doesn't appear in that output
				// then we just skip trying to delete it. Otherwise, we do
				// the Add+Delete below just like in the normal case; the
				// Add *should* be a no-op, but doing it anyway makes this
				// work right even if another thread Deletes the element
				// before we get to it (as long as they don't add it back
				// with a different value).
				typed = findElement(existingMaps[typed.Map], typed.Key)
				if typed == nil {
					continue
				}
			}

			// Do Add+Delete, which ensures the object is deleted whether or
			// not it previously existed.
			tx.Add(typed)
			tx.Delete(typed)
		default:
		}
	}
	return nft.Run(context.TODO(), tx)
}

func findElement(elements []*knftables.Element, key []string) *knftables.Element {
elemLoop:
	for _, elem := range elements {
		if len(elem.Key) != len(key) {
			// All elements have the same key length, so if one fails, they all fail.
			return nil
		}
		for i := range elem.Key {
			if elem.Key[i] != key[i] {
				continue elemLoop
			}
		}
		return elem
	}
	return nil
}
