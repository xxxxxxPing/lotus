# Installing Lotus on Ubuntu

Install these dependencies for Ubuntu.

- go (1.13 or higher)
- gcc (7.4.0 or higher)
- git (version 2 or higher)
- bzr (some go dependency needs this)
- jq
- pkg-config
- opencl-icd-loader
- opencl driver (like nvidia-opencl on arch) (for GPU acceleration)
- opencl-headers (build)
- rustup (proofs build)
- llvm (proofs build)
- clang (proofs build)

Ubuntu / Debian (run):

```sh
sudo apt update
sudo apt install mesa-opencl-icd ocl-icd-opencl-dev
```

Ubuntu (build):

```sh
sudo add-apt-repository ppa:longsleep/golang-backports
sudo apt update
sudo apt install golang-go gcc git bzr jq pkg-config mesa-opencl-icd ocl-icd-opencl-dev
```

Clone

```sh
$ git clone https://github.com/filecoin-project/lotus.git
$ cd lotus/
```

Install

```sh
$ make clean all
$ sudo make install
```

Now you can use the command `lotus` in the command line.
