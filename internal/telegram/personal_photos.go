package telegram

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg"
	_ "image/png"

	"github.com/go-telegram/bot/models"
	"kagari/internal/channel"
)

func (c *PersonalClient) receivePhoto(ctx context.Context, sizes []models.PhotoSize) channel.Attachment {
	// Telegram supplies several sizes of the same photo in a single message.
	largest := sizes[0]
	for _, size := range sizes[1:] {
		area := int64(size.Width) * int64(size.Height)
		largestArea := int64(largest.Width) * int64(largest.Height)
		if area > largestArea || (area == largestArea && size.FileSize > largest.FileSize) {
			largest = size
		}
	}
	return c.receiveFile(ctx, largest.FileID, int64(largest.FileSize), "", "")
}

const maxPhotoBytes = 10_000_000

func photoUploadMIME(data []byte) string {
	if len(data) > maxPhotoBytes {
		return ""
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	// sendPhoto limits the sum of sides to 10,000 and their ratio to 20:1.
	short, long := min(config.Width, config.Height), max(config.Width, config.Height)
	if short <= 0 || long > 10_000-short || long > 20*short {
		return ""
	}
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	default:
		return ""
	}
}
