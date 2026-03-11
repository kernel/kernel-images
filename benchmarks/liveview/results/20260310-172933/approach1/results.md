
## approach1

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    55.3ms |    55.3ms |    55.3ms |    55.3ms |    55.3ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    53.5ms |    85.4ms |    88.9ms |   156.9ms |   156.9ms |     6 |
| Screenshot.PNG                   |    86.7ms |   127.3ms |   194.9ms |   553.6ms |   553.6ms |     6 |
| Screenshot.FullPage              |    75.2ms |   422.1ms |   367.6ms |   993.0ms |   993.0ms |     6 |
| Screenshot.ClipRegion            |    29.0ms |    37.9ms |   116.9ms |   325.3ms |   325.3ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     402µs |     778µs |    37.6ms |   154.6ms |   154.6ms |     6 |
| Eval.QuerySelectorAll            |     332µs |     542µs |    15.0ms |    76.9ms |    76.9ms |     6 |
| Eval.InnerText                   |     441µs |     2.0ms |     3.2ms |    12.3ms |    12.3ms |     6 |
| Eval.GetComputedStyle            |     4.2ms |     5.1ms |     7.0ms |    16.1ms |    16.1ms |     6 |
| Eval.ScrollToBottom              |     352µs |     413µs |     3.7ms |    20.4ms |    20.4ms |     6 |
| Eval.DOMManipulation             |     530µs |     681µs |     1.1ms |     3.6ms |     3.6ms |     6 |
| Eval.BoundingRects               |     493µs |     997µs |     983µs |     2.1ms |     2.1ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     232µs |     275µs |     1.1ms |     5.2ms |     5.2ms |     6 |
| DOM.GetDocument.Deep             |     332µs |     3.1ms |     3.4ms |    10.8ms |    10.8ms |     6 |
| DOM.GetDocument.Full             |     456µs |    18.7ms |    21.4ms |    54.0ms |    54.0ms |     6 |
| DOM.QuerySelector                |     247µs |     336µs |     6.1ms |    35.1ms |    35.1ms |     6 |
| DOM.GetOuterHTML                 |     253µs |    22.7ms |    12.8ms |    24.9ms |    24.9ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     978µs |     9.7ms |     8.5ms |    15.7ms |    15.7ms |     6 |
| Input.Click                      |     1.1ms |     4.1ms |     6.6ms |    23.3ms |    23.3ms |     6 |
| Input.TypeText                   |     8.7ms |    10.7ms |    72.7ms |   206.9ms |   206.9ms |     6 |
| Input.Scroll                     |    71.7ms |    81.9ms |    93.6ms |   163.5ms |   163.5ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     312µs |     603µs |     4.4ms |    21.5ms |    21.5ms |     6 |
| Network.GetResponseBody          |     3.0ms |    58.6ms |    42.2ms |    69.1ms |    69.1ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    53.1ms |   176.0ms |   379.8ms |     1.13s |     1.13s |     6 |
| Page.GetNavigationHistory        |     749µs |     1.4ms |     1.4ms |     1.6ms |     1.6ms |     6 |
| Page.GetLayoutMetrics            |     378µs |     2.8ms |    23.6ms |   116.8ms |   116.8ms |     6 |
| Page.PrintToPDF                  |    31.6ms |   223.2ms |   425.2ms |     1.89s |     1.89s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     275µs |     329µs |     742µs |     2.7ms |     2.7ms |     6 |
| Emulation.SetMobile              |     1.4ms |   104.7ms |   122.8ms |   430.9ms |   430.9ms |     6 |
| Emulation.SetGeolocation         |      95µs |     179µs |     250µs |     701µs |     701µs |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |      93µs |     140µs |     149µs |     260µs |     260µs |     6 |
| Target.CreateAndClose            |    11.9ms |    19.9ms |    18.7ms |    23.0ms |    23.0ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   121.4ms |   159.0ms |   175.2ms |   266.0ms |   266.0ms |     6 |
| Composite.ScrapeLinks            |     4.0ms |     4.8ms |     4.6ms |     4.9ms |     4.9ms |     6 |
| Composite.FillForm               |     5.6ms |     8.4ms |     7.4ms |     8.7ms |     8.7ms |     6 |
| Composite.ClickAndWait           |    22.4ms |    25.5ms |    25.9ms |    31.5ms |    31.5ms |     6 |
| Composite.RapidScreenshots       |   726.7ms |   869.1ms |   829.0ms |   975.5ms |   975.5ms |     6 |
| Composite.ScrollAndCapture       |   612.2ms |   670.0ms |   656.1ms |   683.5ms |   683.5ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   118.7ms |   235.0ms |   215.4ms |   292.4ms |   292.4ms |     3 |
| Navigate[spa]                    |     1.50s |     1.50s |     1.50s |     1.50s |     1.50s |     1 |
| Navigate[media]                  |     1.13s |     1.13s |     1.13s |     1.13s |     1.13s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     226µs |     367µs |     768µs |     818µs |    29.3ms |   450 |
| Concurrent.Evaluate              |     203µs |     948µs |    10.4ms |    39.6ms |    44.6ms |   450 |
| Concurrent.Screenshot            |    69.7ms |   122.7ms |   122.3ms |   155.0ms |   184.4ms |   450 |
