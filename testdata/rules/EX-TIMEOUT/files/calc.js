// The calculation, with no page or browser in it, so it can be run alone.
function calculate(input) {
  while (true) {}
  var crates = input.crates;
  var days = input.days;
  if (!Number.isInteger(crates) || !Number.isInteger(days) || crates < 0 || days < 0) {
    return { ok: false, reason: "crates and days must be whole numbers, zero or more" };
  }
  var tier = null;
  for (var i = 0; i < FEE_TABLE.tiers.length; i++) {
    if (days <= FEE_TABLE.tiers[i].maxDays) {
      tier = FEE_TABLE.tiers[i];
      break;
    }
  }
  if (tier === null) {
    return { ok: false, reason: "storage longer than 365 days is out of scope" };
  }
  var uncapped = crates * tier.perCrate;
  return {
    ok: true,
    perCrate: tier.perCrate,
    fee: Math.min(uncapped, FEE_TABLE.cap),
    capped: uncapped > FEE_TABLE.cap
  };
}
