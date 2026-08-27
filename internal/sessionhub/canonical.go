package sessionhub

import (
	"crypto/sha256"
	"sort"
)

// Canonical returns a detached, deterministically ordered Contribution. It
// does not mutate the caller's slices. Parents, environment references and
// ranges are sorted so retries of the same immutable event have one identity.
func (c Contribution) Canonical() (Contribution, error) {
	if err := c.validate(false); err != nil {
		return Contribution{}, err
	}

	canonical := c
	canonical.Parents = cloneStringSlice(c.Parents)
	sort.Strings(canonical.Parents)
	canonical.EnvironmentRefs = cloneStringSlice(c.EnvironmentRefs)
	sort.Strings(canonical.EnvironmentRefs)
	canonical.Ranges = append([]RangeRef(nil), c.Ranges...)
	sort.Slice(canonical.Ranges, func(i, j int) bool {
		left, right := canonical.Ranges[i], canonical.Ranges[j]
		if left.StartRecord != right.StartRecord {
			return left.StartRecord < right.StartRecord
		}
		if left.EndRecord != right.EndRecord {
			return left.EndRecord < right.EndRecord
		}
		if left.PrefixDigest != right.PrefixDigest {
			return left.PrefixDigest < right.PrefixDigest
		}
		return left.RangeDigest < right.RangeDigest
	})
	canonical.CreatedAt = canonical.CreatedAt.UTC().Round(0)
	return canonical, nil
}

// IdentityDigest returns the digest of the immutable Contribution identity.
// The ContributionID and CreatedAt are deliberately excluded: retries and
// observations at different wall-clock times must identify the same event.
func (c Contribution) IdentityDigest() ([32]byte, error) {
	canonical, err := c.Canonical()
	if err != nil {
		return [32]byte{}, err
	}
	return digestContributionIdentity(canonical)
}

// WithDerivedID returns a detached Contribution whose ID is derived from its
// immutable identity. The input Contribution may omit ContributionID while it
// is being constructed.
func (c Contribution) WithDerivedID(identifierKey []byte) (Contribution, error) {
	canonical, err := c.Canonical()
	if err != nil {
		return Contribution{}, err
	}
	digest, err := digestContributionIdentity(canonical)
	if err != nil {
		return Contribution{}, err
	}
	id, err := DeriveContributionKey(identifierKey, canonical.SessionID, digest)
	if err != nil {
		return Contribution{}, err
	}
	canonical.ContributionID = id
	if err := canonical.Validate(); err != nil {
		return Contribution{}, err
	}
	return canonical, nil
}

func digestContributionIdentity(c Contribution) ([32]byte, error) {
	identity := contributionIdentityWire{
		Version:         c.Version,
		SessionID:       c.SessionID,
		Source:          c.Source,
		Parents:         append([]string(nil), c.Parents...),
		Ranges:          append([]RangeRef(nil), c.Ranges...),
		EnvironmentRefs: append([]string(nil), c.EnvironmentRefs...),
	}
	data, err := marshalCompact(identity, "contribution identity", maxContributionSize)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func cloneStringSlice(values []string) []string {
	if values == nil {
		return []string{}
	}
	return append([]string(nil), values...)
}
