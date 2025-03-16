package images

import (
	"encoding/json"
	"fmt"
	"strings"
)

type minimalCrictlImage struct {
	ID          string   `json:"id"`
	RepoDigests []string `json:"repoDigests"`
}

type minimalCrictlImages struct {
	Images []minimalCrictlImage `json:"images"`
}

func ImageIDByImageURL(desiredImage, jsonData string) (string, error) {
	var imagesData minimalCrictlImages
	if err := json.Unmarshal([]byte(jsonData), &imagesData); err != nil {
		return "", fmt.Errorf("failed to parse crictl JSON: %w", err)
	}

	for _, img := range imagesData.Images {
		for _, repoDigest := range img.RepoDigests {
			if strings.Contains(repoDigest, desiredImage) {
				return img.ID, nil
			}
		}
	}
	return "", nil
}
