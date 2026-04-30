package conf

import (
	"fmt"
	"log"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch installs an fsnotify watcher on filePath and invokes reload with
// a freshly-loaded *Conf each time the file changes. The callback is
// only fired on successful parse — a malformed config.json on a hot
// edit logs an error and is otherwise a no-op (the previous version
// reset *p to defaults and still called the callback, which downstream
// served an empty config and tore down the live runtime).
//
// Two debounces compose: a 10 s "ignore second event within window"
// (typical editor save -> Chmod + Write events) and a 5 s post-debounce
// settle so the writer finishes flushing before we read.
//
// Watch does NOT mutate *p anymore. The caller decides when to commit
// the new config by reading the *Conf passed to the callback. This
// keeps the "old config snapshot" available to the reload orchestrator
// so failure-restore can rebuild from the prior known-good config.
func (p *Conf) Watch(filePath string, reload func(newC *Conf)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("new watcher error: %s", err)
	}
	go func() {
		var pre time.Time
		defer watcher.Close()
		for {
			select {
			case e := <-watcher.Events:
				if e.Has(fsnotify.Chmod) {
					continue
				}
				if pre.Add(10 * time.Second).After(time.Now()) {
					continue
				}
				pre = time.Now()
				go func() {
					time.Sleep(5 * time.Second)
					log.Println("config file changed, parsing...")
					newC := New()
					if err := newC.LoadFromPath(filePath); err != nil {
						log.Printf("reload aborted: parse new config error: %s; old runtime kept active", err)
						return
					}
					reload(newC)
				}()
			case err := <-watcher.Errors:
				if err != nil {
					log.Printf("File watcher error: %s", err)
				}
			}
		}
	}()
	err = watcher.Add(filePath)
	if err != nil {
		return fmt.Errorf("watch file error: %s", err)
	}
	return nil
}
