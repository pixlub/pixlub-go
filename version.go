package pixlub

import "runtime"

// Version is the version of this SDK. It is sent in the User-Agent header.
const Version = "0.3.0"

var userAgent = "pixlub-go/" + Version + " (" + runtime.Version() + ")"
