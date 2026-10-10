/** A small typed event emitter. */
export class Emitter<Events extends { [K in keyof Events]: (...args: any[]) => void }> {
  private listeners = new Map<keyof Events, Set<Events[keyof Events]>>();

  /** Adds a listener and returns a function that removes it. */
  on<K extends keyof Events>(event: K, listener: Events[K]): () => void {
    let set = this.listeners.get(event);
    if (!set) {
      set = new Set();
      this.listeners.set(event, set);
    }
    set.add(listener);
    return () => this.off(event, listener);
  }

  off<K extends keyof Events>(event: K, listener: Events[K]): void {
    this.listeners.get(event)?.delete(listener);
  }

  protected emit<K extends keyof Events>(event: K, ...args: Parameters<Events[K]>): void {
    for (const listener of [...(this.listeners.get(event) ?? [])]) {
      try {
        (listener as (...a: Parameters<Events[K]>) => void)(...args);
      } catch (err) {
        console.error(`[sfu-engine] listener for "${String(event)}" failed`, err);
      }
    }
  }
}
