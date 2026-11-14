// Generated from the .json file of the same name. Do not edit.
globalThis.MOLDLOFT_EXAMPLES = [
  {
    "id": "starting_values",
    "label": "The starting values: 12 at 19.99 for 3 periods",
    "inputs": {
      "periods": "3",
      "quantity": "12",
      "unit_price": "19.99"
    },
    "expect": {
      "amount": "720",
      "per_period": "240.0"
    }
  },
  {
    "id": "half_rounds_up",
    "label": "An amount of exactly one half rounds up to 1",
    "inputs": {
      "periods": "1",
      "quantity": "1",
      "unit_price": "0.50"
    },
    "expect": {
      "amount": "1",
      "per_period": "1.0"
    }
  },
  {
    "id": "twelve_periods",
    "label": "7 at 33.33 for 12 periods",
    "inputs": {
      "periods": "12",
      "quantity": "7",
      "unit_price": "33.33"
    },
    "expect": {
      "amount": "2800",
      "per_period": "233.3"
    }
  }
];
