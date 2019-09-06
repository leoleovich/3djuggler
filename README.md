## Integration with internal website (intern)
You will need to implement internal website API, which supports following:
### /job. Depending on a POST params it should:
  * Give a new job
  * Give details about job
  * Update job status
### /printer. Depending on a POST params it should:
  * Refresh printer healthcheck
  * Reschedule jobs

Every API call must send Username (app) and Password (token) as POST params

## Intergration with printer management device:
Printer management device will need to interact with 3djuggler in order to control the job state.
By default http server will listern on localhost:8888. Juggler supports following blocking API calls:
### /info
Gives information about current job state, printed percentage etc
### /start
Start the job
### /pause
Pause the job
### /cancel
Cancel the job
### /reshedule
Give more time before jobs gets marked as "timed out"
### /version
In order to use this functionality, you needs to compile juggler with extra flag (see [compile](https://github.com/leoleovich/3djuggler#compile) section)

## Compile
Simply run:
```
go get github.com/leoleovich/3djuggler
go build github.com/leoleovich/3djuggler
```
If you want to enable optional `/version` api - you need to build juggler with extra flag:
```
go build -ldflags "-X main.gitCommit=123" github.com/leoleovich/3djuggler
```
Where 123 is a commit hash. You don't have to use this functionality. In this case /version will simply return empty string

## How to run
In order to run juggler you will need a config file. Example of this file you can find in this repository.
Juggler also supports following
There are extra flags you may find useful:
```
  -config string
    Main config (default "3djuggler.json")
  -log string
    Where to log (default "/var/log/3djuggler.log")
  -verbose
    Use verbose log output
```


I am providing code in the repository to you under an open source license. Because this is my personal repository, the license you receive to my code is from me and not my employer (Facebook)
