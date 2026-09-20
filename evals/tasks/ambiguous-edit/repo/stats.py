"""Rolling statistics over a stream of samples."""


class Window:
    def __init__(self, size):
        self.size = size
        self.samples = []
        self.count = 0

    def add(self, value):
        self.samples.append(value)
        self.count += 1
        if len(self.samples) > self.size:
            self.samples.pop(0)

    def mean(self):
        if self.count == 0:
            return 0.0
        return sum(self.samples) / len(self.samples)

    def reset(self):
        self.samples = []
        self.count = 0


def summarise(window):
    return {"count": window.count, "mean": window.mean()}


def tally(values):
    count = 0
    for v in values:
        if v is not None:
            count += 1
    return count


def describe(values):
    return f"{tally(values)} of {len(values)} are set"
