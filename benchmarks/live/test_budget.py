import sys
from decimal import Decimal, localcontext
from pathlib import Path
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import budget


class BudgetArithmeticTest(unittest.TestCase):
    def test_rejects_unknown_excess_precision_and_nonfinite(self):
        for value in ["NaN", "Infinity", "-1", "0", "1e-13", "1e13", "1.123456789012345678901234567"]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                budget.amount(value)
        self.assertEqual(budget.amount("0", zero=True), 0)
        for value in [True, 0, -1, 1_000_000_001]:
            with self.assertRaises(ValueError):
                budget.count(value)

    def test_derived_cap_retains_submicrotoken_fees(self):
        with self.assertRaises(ValueError):
            budget.amount("0.000000000000000001")
        self.assertEqual(budget.amount("0.000000000000000001", computed=True), Decimal("1e-18"))

    def test_admission_does_not_inherit_host_decimal_rounding(self):
        @budget.exact
        def reserve():
            return budget.amount("1.234567890123") * budget.count(999999999) / 1_000_000
        with localcontext() as context:
            context.prec = 4
            self.assertEqual(str(reserve()), "1234.567888888432109877")
            self.assertEqual(context.prec, 4)


if __name__ == "__main__":
    unittest.main()
