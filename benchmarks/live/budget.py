"""Shared exact arithmetic limits for evaluation admission (not provider pricing).

Amounts have at most 24 significant digits and exponents within [-12,12]; token
and request counts are bounded to one billion. With these bounds, 64 decimal
digits cover every multiplication, sum and million-token division in a cohort.
Computed remaining caps may retain up to 18 fractional places after division
by one million; raw approved amounts use the narrower 12-place contract.
Unknown pricing is never supplied by this module.
"""
from decimal import Decimal, InvalidOperation, ROUND_CEILING, localcontext
from functools import wraps


def amount(value, *, zero=False, computed=False):
    try:
        result = Decimal(str(value))
    except (InvalidOperation, ValueError) as exc:
        raise ValueError("known decimal amount required") from exc
    if not result.is_finite() or result < 0 or (not zero and not result):
        raise ValueError("positive amount required (counting fee may explicitly be zero)")
    digits = result.as_tuple()
    if len(digits.digits) > (60 if computed else 24) or not (-18 if computed else -12) <= digits.exponent <= 12:
        raise ValueError("amount precision or magnitude exceeds supported exact arithmetic")
    return result


def count(value):
    if type(value) is not int or not 0 < value <= 1_000_000_000:
        raise ValueError("budget counts must be integers between 1 and one billion")
    return value


def exact(function):
    @wraps(function)
    def wrapped(*args, **kwargs):
        with localcontext() as context:
            context.prec = 64
            context.rounding = ROUND_CEILING
            return function(*args, **kwargs)
    return wrapped
