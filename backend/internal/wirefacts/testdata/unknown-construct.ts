// Fixture for the fail-explicit gate: a namespace block is outside the
// known-construct set, so any generator run covering this file must fail
// naming unknown-construct.ts:2 before emitting anything.
export const KNOWN_SHAPE = 'fine';
namespace Hidden {
  export const X = 1;
}
