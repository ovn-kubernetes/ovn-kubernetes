//go:build linux
// +build linux

package nftables

import (
	"context"

	"sigs.k8s.io/knftables"
)

// UpdateNFTElements adds/updates the given nftables set/map elements. The set or map must
// already exist.
func UpdateNFTElements(elements []*knftables.Element) error {
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()
	for _, elem := range elements {
		tx.Add(elem)
	}
	return nft.Run(context.TODO(), tx)
}

// DeleteNFTElements deletes the given nftables set/map elements. The set or map must
// exist, but if the elements aren't already in the set/map, no error is returned.
//
// To avoid depending on `nft destroy` (which requires kernel 6.3+), this Add()s each
// element before Delete()ing it, so you won't get an error if the element wasn't already
// in the set/map, which means that for map elements, the Value field must be set to the
// correct value. Alternatively, you can leave Value unset, in which case
// DeleteNFTElements will first "list" the map to find its current contents, and then only
// try to delete the elements that are actually present.
func DeleteNFTElements(elements []*knftables.Element) error {
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	// If there are any partial Map Elements, list their maps' existing contents
	existingMaps := make(map[string][]*knftables.Element)
	for _, elem := range elements {
		if elem.Map != "" && len(elem.Value) == 0 && existingMaps[elem.Map] == nil {
			existingElements, err := nft.ListElements(context.TODO(), "map", elem.Map)
			if err != nil {
				return err
			}
			existingMaps[elem.Map] = existingElements
		}
	}

	// Now build the actual transaction
	tx := nft.NewTransaction()
	for _, elem := range elements {
		if elem.Map != "" && len(elem.Value) == 0 {
			// We can't tx.Add() a Map Element with no Value, so try
			// to find its existing value in the List output from
			// above. If the element doesn't appear in that output
			// then we just skip trying to delete it. Otherwise, we do
			// the Add+Delete below just like in the normal case; the
			// Add *should* be a no-op, but doing it anyway makes this
			// work right even if another thread Deletes the element
			// before we get to it (as long as they don't add it back
			// with a different value).
			elem = findElement(existingMaps[elem.Map], elem.Key)
			if elem == nil {
				continue
			}
		}

		// Do Add+Delete, which ensures the object is deleted whether or
		// not it previously existed.
		tx.Add(elem)
		tx.Delete(elem)
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
