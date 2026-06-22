package campaign

import "errors"

// ErrR2ClientRequired is returned when LaunchInstance is called without an R2 client.
var ErrR2ClientRequired = errors.New("R2Assets.Client is required for R2-based bootstrap")

// ErrNoLaunchGroups is returned when LaunchCampaign receives an empty groups list.
var ErrNoLaunchGroups = errors.New("no instance groups to launch")

// ErrNoReplacementOffer is returned when an offer disappears and no replacement
// can be found.
var ErrNoReplacementOffer = errors.New("no replacement offer found")

// ErrDistinctMachinesExhausted is returned when coverage anti-affinity removes
// every otherwise eligible offer.
var ErrDistinctMachinesExhausted = errors.New("all distinct machines covered or in-flight")
