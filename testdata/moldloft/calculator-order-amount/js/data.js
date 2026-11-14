// Generated from the .json file of the same name. Do not edit.
globalThis.MOLDLOFT_TOOL = {
  "contract_version": "v1",
  "template": "calculator",
  "slug": "sample-order-amount",
  "title": "Sample order amount",
  "summary": "Multiplies a made-up quantity, unit price and number of periods into a whole-number amount. It has no fixed values and no lookup tables, to exercise the calculator template without them.",
  "scope_limits": [
    "Every rule and number here is fictional. Do not use the result for a real order."
  ],
  "citations": [
    {
      "id": "rule_amount",
      "label": "Sample Rule 30.1 (fictional)"
    }
  ],
  "inputs": [
    {
      "id": "quantity",
      "label": "Quantity in each period",
      "kind": "number",
      "decimals": 0,
      "min": "0",
      "max": "100000",
      "default": "12"
    },
    {
      "id": "unit_price",
      "label": "Unit price",
      "kind": "number",
      "decimals": 2,
      "min": "0.00",
      "max": "1000.00",
      "default": "19.99"
    },
    {
      "id": "periods",
      "label": "Number of periods",
      "kind": "number",
      "decimals": 0,
      "min": "1",
      "max": "12",
      "default": "3"
    }
  ],
  "constants": [],
  "tables": [],
  "steps": [
    {
      "id": "amount",
      "label": "Order amount",
      "op": "mul",
      "args": [
        "quantity",
        "unit_price",
        "periods"
      ],
      "decimals": 0,
      "citation": "rule_amount"
    },
    {
      "id": "per_period",
      "label": "Amount for each period",
      "op": "div",
      "args": [
        "amount",
        "periods"
      ],
      "decimals": 1
    }
  ],
  "outputs": [
    "amount"
  ]
};
