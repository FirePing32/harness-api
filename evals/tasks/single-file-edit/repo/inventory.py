"""A tiny inventory ledger."""

TAX_RATE = 0.08


def calc_total(items):
    """Sum line items and apply tax."""
    subtotal = sum(item["price"] * item["qty"] for item in items)
    return round(subtotal * (1 + TAX_RATE), 2)


def receipt(items):
    lines = [f'{i["name"]}: {i["price"]} x {i["qty"]}' for i in items]
    lines.append(f"TOTAL: {calc_total(items)}")
    return "\n".join(lines)


def is_over_budget(items, budget):
    return calc_total(items) > budget


if __name__ == "__main__":
    cart = [{"name": "bolt", "price": 0.5, "qty": 10}]
    print(receipt(cart))
    print("over budget" if is_over_budget(cart, 1.0) else "within budget")
