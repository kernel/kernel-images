
## headful

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    28.9ms |    28.9ms |    28.9ms |    28.9ms |    28.9ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    66.7ms |   104.5ms |   102.6ms |   187.6ms |   187.6ms |     6 |
| Screenshot.PNG                   |   133.9ms |   166.7ms |   177.9ms |   272.2ms |   272.2ms |     6 |
| Screenshot.FullPage              |   102.6ms |   395.8ms |   320.5ms |   701.5ms |   701.5ms |     6 |
| Screenshot.ClipRegion            |    76.1ms |    90.3ms |   154.8ms |   371.2ms |   371.2ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     502µs |     671µs |    14.3ms |    66.3ms |    66.3ms |     6 |
| Eval.QuerySelectorAll            |     361µs |     447µs |    14.0ms |    70.7ms |    70.7ms |     6 |
| Eval.InnerText                   |     477µs |     1.0ms |     2.6ms |    10.8ms |    10.8ms |     6 |
| Eval.GetComputedStyle            |     4.1ms |     4.5ms |     5.7ms |    10.6ms |    10.6ms |     6 |
| Eval.ScrollToBottom              |     330µs |     501µs |     666µs |     1.9ms |     1.9ms |     6 |
| Eval.DOMManipulation             |     517µs |     634µs |     682µs |     974µs |     974µs |     6 |
| Eval.BoundingRects               |     427µs |     736µs |     869µs |     1.7ms |     1.7ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     221µs |     287µs |     845µs |     3.7ms |     3.7ms |     6 |
| DOM.GetDocument.Deep             |     308µs |     2.2ms |     2.2ms |     5.6ms |     5.6ms |     6 |
| DOM.GetDocument.Full             |     250µs |    25.4ms |    21.3ms |    53.0ms |    53.0ms |     6 |
| DOM.QuerySelector                |     225µs |     317µs |     1.3ms |     6.3ms |     6.3ms |     6 |
| DOM.GetOuterHTML                 |     238µs |    23.0ms |    14.8ms |    32.3ms |    32.3ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     2.0ms |    12.0ms |    12.3ms |    23.9ms |    23.9ms |     6 |
| Input.Click                      |     1.2ms |     3.3ms |     7.1ms |    28.4ms |    28.4ms |     6 |
| Input.TypeText                   |     8.7ms |     9.8ms |    25.8ms |   100.3ms |   100.3ms |     6 |
| Input.Scroll                     |   188.5ms |   198.3ms |   345.4ms |   999.5ms |   999.5ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     260µs |     505µs |     542µs |     1.1ms |     1.1ms |     6 |
| Network.GetResponseBody          |     2.8ms |    65.8ms |    52.8ms |   108.3ms |   108.3ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    33.5ms |   163.6ms |   275.8ms |   748.8ms |   748.8ms |     6 |
| Page.GetNavigationHistory        |     796µs |     1.1ms |     1.0ms |     1.2ms |     1.2ms |     6 |
| Page.GetLayoutMetrics            |     220µs |     7.0ms |     5.7ms |    15.0ms |    15.0ms |     6 |
| Page.PrintToPDF                  |    30.4ms |   242.0ms |   252.6ms |   820.5ms |   820.5ms |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     195µs |     418µs |     371µs |     516µs |     516µs |     6 |
| Emulation.SetMobile              |     1.2ms |   101.3ms |   113.0ms |   404.6ms |   404.6ms |     6 |
| Emulation.SetGeolocation         |     128µs |     219µs |     206µs |     315µs |     315µs |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     114µs |     163µs |     191µs |     375µs |     375µs |     6 |
| Target.CreateAndClose            |    12.6ms |    17.2ms |    17.5ms |    27.2ms |    27.2ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   101.0ms |   274.3ms |   220.6ms |   289.5ms |   289.5ms |     6 |
| Composite.ScrapeLinks            |     3.8ms |     4.6ms |     4.5ms |     5.3ms |     5.3ms |     6 |
| Composite.FillForm               |     5.3ms |     6.1ms |     6.5ms |     8.1ms |     8.1ms |     6 |
| Composite.ClickAndWait           |    17.6ms |    21.7ms |    21.1ms |    24.8ms |    24.8ms |     6 |
| Composite.RapidScreenshots       |     1.04s |     1.16s |     1.13s |     1.30s |     1.30s |     6 |
| Composite.ScrollAndCapture       |   597.4ms |   639.4ms |   621.2ms |   649.3ms |   649.3ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   116.8ms |   208.1ms |   185.8ms |   232.6ms |   232.6ms |     3 |
| Navigate[spa]                    |     1.27s |     1.27s |     1.27s |     1.27s |     1.27s |     1 |
| Navigate[media]                  |   958.6ms |   958.6ms |   958.6ms |   958.6ms |   958.6ms |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     225µs |     327µs |     425µs |     661µs |    19.9ms |   391 |
| Concurrent.Evaluate              |     237µs |     504µs |     3.6ms |    19.5ms |    22.3ms |   391 |
| Concurrent.Screenshot            |    95.7ms |   151.1ms |   150.0ms |   179.5ms |   225.9ms |   391 |
