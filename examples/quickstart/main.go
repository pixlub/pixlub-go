// Command quickstart lists the tools of the Pixlub API, and runs one on an image.
//
//	export PIXLUB_API_KEY="pix.xxxxxxxx.xxxxxxxx..."
//	go run ./examples/quickstart            # list the tools
//	go run ./examples/quickstart photo.jpg  # remove the background of photo.jpg
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	pixlub "github.com/pixlub/pixlub-go"
)

func main() {
	client, err := pixlub.NewClient()
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if len(os.Args) < 2 {
		tools, err := client.Tools.List(ctx, pixlub.ListOptions{ExecutableOnly: true})
		if err != nil {
			log.Fatal(err)
		}
		for _, tool := range tools {
			fmt.Printf("%-24s %s\n", tool.ID, tool.Name)
		}
		return
	}

	result, err := client.Tools.Run(ctx, "removeBgApi", pixlub.RunRequest{
		Image: pixlub.FromPath(os.Args[1]),
	})
	if err != nil {
		log.Fatal(err)
	}

	name := "photo-nobg" + result.Extension()
	if err := result.Save(name, false); err != nil {
		log.Fatal(err)
	}
	fmt.Println("saved", name)
}
