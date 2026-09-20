"""Entry point for the scheduler."""

from queue import Queue

WORKERS = 4


def enqueue(q: Queue, job):
    q.put(job)


def drain(q: Queue):
    out = []
    while not q.empty():
        out.append(q.get())
    return out


if __name__ == "__main__":
    q = Queue()
    for n in range(10):
        enqueue(q, n)
    print(drain(q))
