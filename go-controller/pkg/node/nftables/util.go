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
func DeleteNFTElements(elements []*knftables.Element) error {
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()
	for _, elem := range elements {
		tx.Destroy(elem)
	}
	return nft.Run(context.TODO(), tx)
}
