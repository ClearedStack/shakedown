// Generated from the .json file of the same name. Do not edit.
globalThis.MOLDLOFT_EXAMPLES = [
  {
    "id": "starting_values",
    "label": "The starting values: 40.00 an hour, 100 hours, standard fee tier",
    "inputs": {
      "base_rate": "40.00",
      "fee_tier": "standard",
      "hours": "100"
    },
    "expect": {
      "above_floor": "7.37",
      "billable_rate": "67.37",
      "cost_rate": "62.67",
      "days": "12.5",
      "fee": "4.70",
      "fee_uncapped": "4.70",
      "fringe": "13.00",
      "loaded_rate": "67.37",
      "overhead": "9.67",
      "overhead_base": "53.00",
      "total": "6737.00"
    }
  },
  {
    "id": "fee_cap_applies",
    "label": "A high rate on the high-risk tier, where the fee cap applies",
    "inputs": {
      "base_rate": "123.45",
      "fee_tier": "high",
      "hours": "37.5"
    },
    "expect": {
      "billable_rate": "198.42",
      "cost_rate": "193.42",
      "days": "4.7",
      "fee": "5.00",
      "fee_uncapped": "19.34",
      "fringe": "40.12",
      "overhead": "29.85",
      "total": "7440.75"
    }
  },
  {
    "id": "floor_applies",
    "label": "A low rate, where the minimum billable rate applies",
    "inputs": {
      "base_rate": "30.00",
      "fee_tier": "low",
      "hours": "7.5"
    },
    "expect": {
      "above_floor": "-10.65",
      "billable_rate": "60.00",
      "cost_rate": "47.00",
      "days": "0.9",
      "fee": "2.35",
      "fringe": "9.75",
      "loaded_rate": "49.35",
      "overhead": "7.25",
      "total": "450.00"
    }
  },
  {
    "id": "all_zero",
    "label": "The lowest values allowed: nothing an hour and no hours",
    "inputs": {
      "base_rate": "0.00",
      "fee_tier": "standard",
      "hours": "0"
    },
    "expect": {
      "above_floor": "-60.00",
      "billable_rate": "60.00",
      "cost_rate": "0.00",
      "days": "0.0",
      "total": "0.00"
    }
  },
  {
    "id": "half_rounds_up",
    "label": "A rate chosen so that the fringe lands exactly on a half: 0.20 an hour",
    "inputs": {
      "base_rate": "0.20",
      "fee_tier": "standard",
      "hours": "1"
    },
    "expect": {
      "billable_rate": "60.00",
      "cost_rate": "0.32",
      "days": "0.1",
      "fee": "0.02",
      "fringe": "0.07",
      "overhead": "0.05",
      "total": "60.00"
    }
  }
];
