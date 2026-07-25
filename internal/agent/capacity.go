package agent

// admitClass decides whether one more run of class may be admitted into the
// shared pool, implementing reserve-with-borrowing: each class in floors gets
// a guaranteed minimum concurrent slot count, but an idle class's unused
// guarantee can be borrowed by any other class rather than sitting stranded.
//
// live and reserved are per-class counts of currently running agents and held
// capacity reservations respectively; floors is the configured per-class
// minimum (agent.class_reservations); max is the pool ceiling
// (agent.max_concurrent, or an SLO-narrowed ceiling below it).
//
// max<=0 always admits (unlimited pool). Otherwise: admission requires free
// pool capacity (total used < max), and either class hasn't yet reached its
// own floor (its guarantee always wins while any capacity remains), or there
// is enough unprotected capacity left after reserving every other
// below-floor class's remaining deficit.
//
// With every floor at zero (the default, unconfigured state) this reduces
// exactly to the pre-class rule "total used < max" — see TestAdmitClass's
// zero-floors case, which is the contract for zero-regression rollout.
func admitClass(class WorkloadClass, live, reserved, floors map[WorkloadClass]int, poolMax int) bool {
	if poolMax <= 0 {
		return true
	}

	used := make(map[WorkloadClass]int, len(AllWorkloadClasses()))
	var total int
	for _, c := range AllWorkloadClasses() {
		u := live[c] + reserved[c]
		used[c] = u
		total += u
	}

	if total >= poolMax {
		return false
	}
	if used[class] < floors[class] {
		return true
	}

	var protected int
	for _, d := range AllWorkloadClasses() {
		if d == class {
			continue
		}
		if deficit := floors[d] - used[d]; deficit > 0 {
			protected += deficit
		}
	}
	return total+protected < poolMax
}

// borrowsSharedCapacity reports whether an admission of class draws on
// capacity beyond its own reserved floor — i.e. it only proceeds because
// another class's guarantee is currently unused, not because of class's own
// guarantee. Used only to label the "borrowed" metric at the admission gate;
// admitClass's admit/reject decision does not depend on this. Reports false
// whenever no class_reservations are configured at all (floors all zero),
// since there is then nothing to borrow from — "borrowed" only has meaning
// once floors exist.
func borrowsSharedCapacity(class WorkloadClass, live, reserved, floors map[WorkloadClass]int) bool {
	var floorSum int
	for _, c := range AllWorkloadClasses() {
		floorSum += floors[c]
	}
	if floorSum == 0 {
		return false
	}
	return live[class]+reserved[class] >= floors[class]
}
