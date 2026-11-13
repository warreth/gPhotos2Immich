package app

import (
	"context"

	"warreth.dev/gphotos2immich/pkg/googlephotos"
	"warreth.dev/gphotos2immich/pkg/immich"
	"warreth.dev/gphotos2immich/pkg/syncback"
)

// scraperResolver adapts the public album scraper to the syncback module.
// Keeping the adapter here means a scraper repair never edits syncback.
type scraperResolver struct {
	client *googlephotos.Client
}

func (r scraperResolver) Resolve(ctx context.Context, shareURL string) (syncback.AlbumInfo, error) {
	album, err := googlephotos.ScrapeAlbum(ctx, r.client, shareURL)
	if err != nil {
		return syncback.AlbumInfo{}, err
	}
	return syncback.AlbumInfo{
		MediaKey: album.MediaKey,
		Title:    album.Title,
		Items:    len(album.Photos),
	}, nil
}

// immichAlbumLister adapts the Immich client to the syncback module.
type immichAlbumLister struct {
	client *immich.Client
}

func (l immichAlbumLister) Albums(ctx context.Context) ([]syncback.Club, error) {
	albums, err := l.client.ListAlbums(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]syncback.Club, 0, len(albums))
	for _, album := range albums {
		out = append(out, syncback.Club{
			ID:    album.Id,
			Name:  album.AlbumName,
			Items: len(album.Assets),
		})
	}
	return out, nil
}
