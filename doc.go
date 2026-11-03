// Package pixlub is the official Go client for the Pixlub API.
//
//	import pixlub "github.com/pixlub/pixlub-go"
//
//	client, err := pixlub.NewClient() // reads PIXLUB_API_KEY
//	if err != nil {
//		log.Fatal(err)
//	}
//	result, err := client.Tools.Run(ctx, "removeBgApi", pixlub.RunRequest{
//		Image: pixlub.FromPath("photo.jpg"),
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	if err := result.Save("photo-nobg"+result.Extension(), false); err != nil {
//		log.Fatal(err)
//	}
//
// A Client is safe for concurrent use: create one and reuse it. Large files are
// uploaded automatically, and long runs are awaited until WithJobTimeout.
// Arguments are checked before anything is sent, so a mistake costs nothing.
package pixlub
