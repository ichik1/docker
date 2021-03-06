// DEPRECATION NOTICE. PLEASE DO NOT ADD ANYTHING TO THIS FILE.
//
// For additional commments see server/server.go
//
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/archive"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/graph"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/parsers/filters"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/utils"
)

// ImageExport exports all images with the given tag. All versions
// containing the same tag are exported. The resulting output is an
// uncompressed tar ball.
// name is the set of tags to export.
// out is the writer where the images are written to.
func (srv *Server) ImageExport(job *engine.Job) engine.Status {
	if len(job.Args) != 1 {
		return job.Errorf("Usage: %s IMAGE\n", job.Name)
	}
	name := job.Args[0]
	// get image json
	tempdir, err := ioutil.TempDir("", "docker-export-")
	if err != nil {
		return job.Error(err)
	}
	defer os.RemoveAll(tempdir)

	utils.Debugf("Serializing %s", name)

	rootRepoMap := map[string]graph.Repository{}
	rootRepo, err := srv.daemon.Repositories().Get(name)
	if err != nil {
		return job.Error(err)
	}
	if rootRepo != nil {
		// this is a base repo name, like 'busybox'

		for _, id := range rootRepo {
			if err := srv.exportImage(job.Eng, id, tempdir); err != nil {
				return job.Error(err)
			}
		}
		rootRepoMap[name] = rootRepo
	} else {
		img, err := srv.daemon.Repositories().LookupImage(name)
		if err != nil {
			return job.Error(err)
		}
		if img != nil {
			// This is a named image like 'busybox:latest'
			repoName, repoTag := parsers.ParseRepositoryTag(name)
			if err := srv.exportImage(job.Eng, img.ID, tempdir); err != nil {
				return job.Error(err)
			}
			// check this length, because a lookup of a truncated has will not have a tag
			// and will not need to be added to this map
			if len(repoTag) > 0 {
				rootRepoMap[repoName] = graph.Repository{repoTag: img.ID}
			}
		} else {
			// this must be an ID that didn't get looked up just right?
			if err := srv.exportImage(job.Eng, name, tempdir); err != nil {
				return job.Error(err)
			}
		}
	}
	// write repositories, if there is something to write
	if len(rootRepoMap) > 0 {
		rootRepoJson, _ := json.Marshal(rootRepoMap)

		if err := ioutil.WriteFile(path.Join(tempdir, "repositories"), rootRepoJson, os.FileMode(0644)); err != nil {
			return job.Error(err)
		}
	} else {
		utils.Debugf("There were no repositories to write")
	}

	fs, err := archive.Tar(tempdir, archive.Uncompressed)
	if err != nil {
		return job.Error(err)
	}
	defer fs.Close()

	if _, err := io.Copy(job.Stdout, fs); err != nil {
		return job.Error(err)
	}
	utils.Debugf("End Serializing %s", name)
	return engine.StatusOK
}

func (srv *Server) exportImage(eng *engine.Engine, name, tempdir string) error {
	for n := name; n != ""; {
		// temporary directory
		tmpImageDir := path.Join(tempdir, n)
		if err := os.Mkdir(tmpImageDir, os.FileMode(0755)); err != nil {
			if os.IsExist(err) {
				return nil
			}
			return err
		}

		var version = "1.0"
		var versionBuf = []byte(version)

		if err := ioutil.WriteFile(path.Join(tmpImageDir, "VERSION"), versionBuf, os.FileMode(0644)); err != nil {
			return err
		}

		// serialize json
		json, err := os.Create(path.Join(tmpImageDir, "json"))
		if err != nil {
			return err
		}
		job := eng.Job("image_inspect", n)
		job.SetenvBool("raw", true)
		job.Stdout.Add(json)
		if err := job.Run(); err != nil {
			return err
		}

		// serialize filesystem
		fsTar, err := os.Create(path.Join(tmpImageDir, "layer.tar"))
		if err != nil {
			return err
		}
		job = eng.Job("image_tarlayer", n)
		job.Stdout.Add(fsTar)
		if err := job.Run(); err != nil {
			return err
		}

		// find parent
		job = eng.Job("image_get", n)
		info, _ := job.Stdout.AddEnv()
		if err := job.Run(); err != nil {
			return err
		}
		n = info.Get("Parent")
	}
	return nil
}

func (srv *Server) Build(job *engine.Job) engine.Status {
	if len(job.Args) != 0 {
		return job.Errorf("Usage: %s\n", job.Name)
	}
	var (
		remoteURL      = job.Getenv("remote")
		repoName       = job.Getenv("t")
		suppressOutput = job.GetenvBool("q")
		noCache        = job.GetenvBool("nocache")
		rm             = job.GetenvBool("rm")
		forceRm        = job.GetenvBool("forcerm")
		authConfig     = &registry.AuthConfig{}
		configFile     = &registry.ConfigFile{}
		tag            string
		context        io.ReadCloser
	)
	job.GetenvJson("authConfig", authConfig)
	job.GetenvJson("configFile", configFile)
	repoName, tag = parsers.ParseRepositoryTag(repoName)

	if remoteURL == "" {
		context = ioutil.NopCloser(job.Stdin)
	} else if utils.IsGIT(remoteURL) {
		if !strings.HasPrefix(remoteURL, "git://") {
			remoteURL = "https://" + remoteURL
		}
		root, err := ioutil.TempDir("", "docker-build-git")
		if err != nil {
			return job.Error(err)
		}
		defer os.RemoveAll(root)

		if output, err := exec.Command("git", "clone", "--recursive", remoteURL, root).CombinedOutput(); err != nil {
			return job.Errorf("Error trying to use git: %s (%s)", err, output)
		}

		c, err := archive.Tar(root, archive.Uncompressed)
		if err != nil {
			return job.Error(err)
		}
		context = c
	} else if utils.IsURL(remoteURL) {
		f, err := utils.Download(remoteURL)
		if err != nil {
			return job.Error(err)
		}
		defer f.Body.Close()
		dockerFile, err := ioutil.ReadAll(f.Body)
		if err != nil {
			return job.Error(err)
		}
		c, err := archive.Generate("Dockerfile", string(dockerFile))
		if err != nil {
			return job.Error(err)
		}
		context = c
	}
	defer context.Close()

	sf := utils.NewStreamFormatter(job.GetenvBool("json"))
	b := builder.NewBuildFile(srv.daemon, srv.Eng,
		&utils.StdoutFormater{
			Writer:          job.Stdout,
			StreamFormatter: sf,
		},
		&utils.StderrFormater{
			Writer:          job.Stdout,
			StreamFormatter: sf,
		},
		!suppressOutput, !noCache, rm, forceRm, job.Stdout, sf, authConfig, configFile)
	id, err := b.Build(context)
	if err != nil {
		return job.Error(err)
	}
	if repoName != "" {
		srv.daemon.Repositories().Set(repoName, tag, id, false)
	}
	return engine.StatusOK
}

// Loads a set of images into the repository. This is the complementary of ImageExport.
// The input stream is an uncompressed tar ball containing images and metadata.
func (srv *Server) ImageLoad(job *engine.Job) engine.Status {
	tmpImageDir, err := ioutil.TempDir("", "docker-import-")
	if err != nil {
		return job.Error(err)
	}
	defer os.RemoveAll(tmpImageDir)

	var (
		repoTarFile = path.Join(tmpImageDir, "repo.tar")
		repoDir     = path.Join(tmpImageDir, "repo")
	)

	tarFile, err := os.Create(repoTarFile)
	if err != nil {
		return job.Error(err)
	}
	if _, err := io.Copy(tarFile, job.Stdin); err != nil {
		return job.Error(err)
	}
	tarFile.Close()

	repoFile, err := os.Open(repoTarFile)
	if err != nil {
		return job.Error(err)
	}
	if err := os.Mkdir(repoDir, os.ModeDir); err != nil {
		return job.Error(err)
	}
	if err := archive.Untar(repoFile, repoDir, nil); err != nil {
		return job.Error(err)
	}

	dirs, err := ioutil.ReadDir(repoDir)
	if err != nil {
		return job.Error(err)
	}

	for _, d := range dirs {
		if d.IsDir() {
			if err := srv.recursiveLoad(job.Eng, d.Name(), tmpImageDir); err != nil {
				return job.Error(err)
			}
		}
	}

	repositoriesJson, err := ioutil.ReadFile(path.Join(tmpImageDir, "repo", "repositories"))
	if err == nil {
		repositories := map[string]graph.Repository{}
		if err := json.Unmarshal(repositoriesJson, &repositories); err != nil {
			return job.Error(err)
		}

		for imageName, tagMap := range repositories {
			for tag, address := range tagMap {
				if err := srv.daemon.Repositories().Set(imageName, tag, address, true); err != nil {
					return job.Error(err)
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return job.Error(err)
	}

	return engine.StatusOK
}

func (srv *Server) recursiveLoad(eng *engine.Engine, address, tmpImageDir string) error {
	if err := eng.Job("image_get", address).Run(); err != nil {
		utils.Debugf("Loading %s", address)

		imageJson, err := ioutil.ReadFile(path.Join(tmpImageDir, "repo", address, "json"))
		if err != nil {
			utils.Debugf("Error reading json", err)
			return err
		}

		layer, err := os.Open(path.Join(tmpImageDir, "repo", address, "layer.tar"))
		if err != nil {
			utils.Debugf("Error reading embedded tar", err)
			return err
		}
		img, err := image.NewImgJSON(imageJson)
		if err != nil {
			utils.Debugf("Error unmarshalling json", err)
			return err
		}
		if img.Parent != "" {
			if !srv.daemon.Graph().Exists(img.Parent) {
				if err := srv.recursiveLoad(eng, img.Parent, tmpImageDir); err != nil {
					return err
				}
			}
		}
		if err := srv.daemon.Graph().Register(imageJson, layer, img); err != nil {
			return err
		}
	}
	utils.Debugf("Completed processing %s", address)

	return nil
}

func (srv *Server) ImagesViz(job *engine.Job) engine.Status {
	images, _ := srv.daemon.Graph().Map()
	if images == nil {
		return engine.StatusOK
	}
	job.Stdout.Write([]byte("digraph docker {\n"))

	var (
		parentImage *image.Image
		err         error
	)
	for _, image := range images {
		parentImage, err = image.GetParent()
		if err != nil {
			return job.Errorf("Error while getting parent image: %v", err)
		}
		if parentImage != nil {
			job.Stdout.Write([]byte(" \"" + parentImage.ID + "\" -> \"" + image.ID + "\"\n"))
		} else {
			job.Stdout.Write([]byte(" base -> \"" + image.ID + "\" [style=invis]\n"))
		}
	}

	for id, repos := range srv.daemon.Repositories().GetRepoRefs() {
		job.Stdout.Write([]byte(" \"" + id + "\" [label=\"" + id + "\\n" + strings.Join(repos, "\\n") + "\",shape=box,fillcolor=\"paleturquoise\",style=\"filled,rounded\"];\n"))
	}
	job.Stdout.Write([]byte(" base [style=invisible]\n}\n"))
	return engine.StatusOK
}

func (srv *Server) Images(job *engine.Job) engine.Status {
	var (
		allImages   map[string]*image.Image
		err         error
		filt_tagged = true
	)

	imageFilters, err := filters.FromParam(job.Getenv("filters"))
	if err != nil {
		return job.Error(err)
	}
	if i, ok := imageFilters["dangling"]; ok {
		for _, value := range i {
			if strings.ToLower(value) == "true" {
				filt_tagged = false
			}
		}
	}

	if job.GetenvBool("all") && filt_tagged {
		allImages, err = srv.daemon.Graph().Map()
	} else {
		allImages, err = srv.daemon.Graph().Heads()
	}
	if err != nil {
		return job.Error(err)
	}
	lookup := make(map[string]*engine.Env)
	srv.daemon.Repositories().Lock()
	for name, repository := range srv.daemon.Repositories().Repositories {
		if job.Getenv("filter") != "" {
			if match, _ := path.Match(job.Getenv("filter"), name); !match {
				continue
			}
		}
		for tag, id := range repository {
			image, err := srv.daemon.Graph().Get(id)
			if err != nil {
				log.Printf("Warning: couldn't load %s from %s/%s: %s", id, name, tag, err)
				continue
			}

			if out, exists := lookup[id]; exists {
				if filt_tagged {
					out.SetList("RepoTags", append(out.GetList("RepoTags"), fmt.Sprintf("%s:%s", name, tag)))
				}
			} else {
				// get the boolean list for if only the untagged images are requested
				delete(allImages, id)
				if filt_tagged {
					out := &engine.Env{}
					out.Set("ParentId", image.Parent)
					out.SetList("RepoTags", []string{fmt.Sprintf("%s:%s", name, tag)})
					out.Set("Id", image.ID)
					out.SetInt64("Created", image.Created.Unix())
					out.SetInt64("Size", image.Size)
					out.SetInt64("VirtualSize", image.GetParentsSize(0)+image.Size)
					lookup[id] = out
				}
			}

		}
	}
	srv.daemon.Repositories().Unlock()

	outs := engine.NewTable("Created", len(lookup))
	for _, value := range lookup {
		outs.Add(value)
	}

	// Display images which aren't part of a repository/tag
	if job.Getenv("filter") == "" {
		for _, image := range allImages {
			out := &engine.Env{}
			out.Set("ParentId", image.Parent)
			out.SetList("RepoTags", []string{"<none>:<none>"})
			out.Set("Id", image.ID)
			out.SetInt64("Created", image.Created.Unix())
			out.SetInt64("Size", image.Size)
			out.SetInt64("VirtualSize", image.GetParentsSize(0)+image.Size)
			outs.Add(out)
		}
	}

	outs.ReverseSort()
	if _, err := outs.WriteListTo(job.Stdout); err != nil {
		return job.Error(err)
	}
	return engine.StatusOK
}

func (srv *Server) ImageHistory(job *engine.Job) engine.Status {
	if n := len(job.Args); n != 1 {
		return job.Errorf("Usage: %s IMAGE", job.Name)
	}
	name := job.Args[0]
	foundImage, err := srv.daemon.Repositories().LookupImage(name)
	if err != nil {
		return job.Error(err)
	}

	lookupMap := make(map[string][]string)
	for name, repository := range srv.daemon.Repositories().Repositories {
		for tag, id := range repository {
			// If the ID already has a reverse lookup, do not update it unless for "latest"
			if _, exists := lookupMap[id]; !exists {
				lookupMap[id] = []string{}
			}
			lookupMap[id] = append(lookupMap[id], name+":"+tag)
		}
	}

	outs := engine.NewTable("Created", 0)
	err = foundImage.WalkHistory(func(img *image.Image) error {
		out := &engine.Env{}
		out.Set("Id", img.ID)
		out.SetInt64("Created", img.Created.Unix())
		out.Set("CreatedBy", strings.Join(img.ContainerConfig.Cmd, " "))
		out.SetList("Tags", lookupMap[img.ID])
		out.SetInt64("Size", img.Size)
		outs.Add(out)
		return nil
	})
	if _, err := outs.WriteListTo(job.Stdout); err != nil {
		return job.Error(err)
	}
	return engine.StatusOK
}
func (srv *Server) ImageTag(job *engine.Job) engine.Status {
	if len(job.Args) != 2 && len(job.Args) != 3 {
		return job.Errorf("Usage: %s IMAGE REPOSITORY [TAG]\n", job.Name)
	}
	var tag string
	if len(job.Args) == 3 {
		tag = job.Args[2]
	}
	if err := srv.daemon.Repositories().Set(job.Args[1], tag, job.Args[0], job.GetenvBool("force")); err != nil {
		return job.Error(err)
	}
	return engine.StatusOK
}

func (srv *Server) pullImage(r *registry.Registry, out io.Writer, imgID, endpoint string, token []string, sf *utils.StreamFormatter) error {
	history, err := r.GetRemoteHistory(imgID, endpoint, token)
	if err != nil {
		return err
	}
	out.Write(sf.FormatProgress(utils.TruncateID(imgID), "Pulling dependent layers", nil))
	// FIXME: Try to stream the images?
	// FIXME: Launch the getRemoteImage() in goroutines

	for i := len(history) - 1; i >= 0; i-- {
		id := history[i]

		// ensure no two downloads of the same layer happen at the same time
		if c, err := srv.poolAdd("pull", "layer:"+id); err != nil {
			utils.Debugf("Image (id: %s) pull is already running, skipping: %v", id, err)
			<-c
		}
		defer srv.poolRemove("pull", "layer:"+id)

		if !srv.daemon.Graph().Exists(id) {
			out.Write(sf.FormatProgress(utils.TruncateID(id), "Pulling metadata", nil))
			var (
				imgJSON []byte
				imgSize int
				err     error
				img     *image.Image
			)
			retries := 5
			for j := 1; j <= retries; j++ {
				imgJSON, imgSize, err = r.GetRemoteImageJSON(id, endpoint, token)
				if err != nil && j == retries {
					out.Write(sf.FormatProgress(utils.TruncateID(id), "Error pulling dependent layers", nil))
					return err
				} else if err != nil {
					time.Sleep(time.Duration(j) * 500 * time.Millisecond)
					continue
				}
				img, err = image.NewImgJSON(imgJSON)
				if err != nil && j == retries {
					out.Write(sf.FormatProgress(utils.TruncateID(id), "Error pulling dependent layers", nil))
					return fmt.Errorf("Failed to parse json: %s", err)
				} else if err != nil {
					time.Sleep(time.Duration(j) * 500 * time.Millisecond)
					continue
				} else {
					break
				}
			}

			for j := 1; j <= retries; j++ {
				// Get the layer
				status := "Pulling fs layer"
				if j > 1 {
					status = fmt.Sprintf("Pulling fs layer [retries: %d]", j)
				}
				out.Write(sf.FormatProgress(utils.TruncateID(id), status, nil))
				layer, err := r.GetRemoteImageLayer(img.ID, endpoint, token, int64(imgSize))
				if uerr, ok := err.(*url.Error); ok {
					err = uerr.Err
				}
				if terr, ok := err.(net.Error); ok && terr.Timeout() && j < retries {
					time.Sleep(time.Duration(j) * 500 * time.Millisecond)
					continue
				} else if err != nil {
					out.Write(sf.FormatProgress(utils.TruncateID(id), "Error pulling dependent layers", nil))
					return err
				}
				defer layer.Close()

				err = srv.daemon.Graph().Register(imgJSON,
					utils.ProgressReader(layer, imgSize, out, sf, false, utils.TruncateID(id), "Downloading"),
					img)
				if terr, ok := err.(net.Error); ok && terr.Timeout() && j < retries {
					time.Sleep(time.Duration(j) * 500 * time.Millisecond)
					continue
				} else if err != nil {
					out.Write(sf.FormatProgress(utils.TruncateID(id), "Error downloading dependent layers", nil))
					return err
				} else {
					break
				}
			}
		}
		out.Write(sf.FormatProgress(utils.TruncateID(id), "Download complete", nil))

	}
	return nil
}

func (srv *Server) pullRepository(r *registry.Registry, out io.Writer, localName, remoteName, askedTag string, sf *utils.StreamFormatter, parallel bool) error {
	out.Write(sf.FormatStatus("", "Pulling repository %s", localName))

	repoData, err := r.GetRepositoryData(remoteName)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP code: 404") {
			return fmt.Errorf("Error: image %s not found", remoteName)
		} else {
			// Unexpected HTTP error
			return err
		}
	}

	utils.Debugf("Retrieving the tag list")
	tagsList, err := r.GetRemoteTags(repoData.Endpoints, remoteName, repoData.Tokens)
	if err != nil {
		utils.Errorf("%v", err)
		return err
	}

	for tag, id := range tagsList {
		repoData.ImgList[id] = &registry.ImgData{
			ID:       id,
			Tag:      tag,
			Checksum: "",
		}
	}

	utils.Debugf("Registering tags")
	// If no tag has been specified, pull them all
	if askedTag == "" {
		for tag, id := range tagsList {
			repoData.ImgList[id].Tag = tag
		}
	} else {
		// Otherwise, check that the tag exists and use only that one
		id, exists := tagsList[askedTag]
		if !exists {
			return fmt.Errorf("Tag %s not found in repository %s", askedTag, localName)
		}
		repoData.ImgList[id].Tag = askedTag
	}

	errors := make(chan error)
	for _, image := range repoData.ImgList {
		downloadImage := func(img *registry.ImgData) {
			if askedTag != "" && img.Tag != askedTag {
				utils.Debugf("(%s) does not match %s (id: %s), skipping", img.Tag, askedTag, img.ID)
				if parallel {
					errors <- nil
				}
				return
			}

			if img.Tag == "" {
				utils.Debugf("Image (id: %s) present in this repository but untagged, skipping", img.ID)
				if parallel {
					errors <- nil
				}
				return
			}

			// ensure no two downloads of the same image happen at the same time
			if c, err := srv.poolAdd("pull", "img:"+img.ID); err != nil {
				if c != nil {
					out.Write(sf.FormatProgress(utils.TruncateID(img.ID), "Layer already being pulled by another client. Waiting.", nil))
					<-c
					out.Write(sf.FormatProgress(utils.TruncateID(img.ID), "Download complete", nil))
				} else {
					utils.Debugf("Image (id: %s) pull is already running, skipping: %v", img.ID, err)
				}
				if parallel {
					errors <- nil
				}
				return
			}
			defer srv.poolRemove("pull", "img:"+img.ID)

			out.Write(sf.FormatProgress(utils.TruncateID(img.ID), fmt.Sprintf("Pulling image (%s) from %s", img.Tag, localName), nil))
			success := false
			var lastErr error
			for _, ep := range repoData.Endpoints {
				out.Write(sf.FormatProgress(utils.TruncateID(img.ID), fmt.Sprintf("Pulling image (%s) from %s, endpoint: %s", img.Tag, localName, ep), nil))
				if err := srv.pullImage(r, out, img.ID, ep, repoData.Tokens, sf); err != nil {
					// It's not ideal that only the last error is returned, it would be better to concatenate the errors.
					// As the error is also given to the output stream the user will see the error.
					lastErr = err
					out.Write(sf.FormatProgress(utils.TruncateID(img.ID), fmt.Sprintf("Error pulling image (%s) from %s, endpoint: %s, %s", img.Tag, localName, ep, err), nil))
					continue
				}
				success = true
				break
			}
			if !success {
				err := fmt.Errorf("Error pulling image (%s) from %s, %v", img.Tag, localName, lastErr)
				out.Write(sf.FormatProgress(utils.TruncateID(img.ID), err.Error(), nil))
				if parallel {
					errors <- err
					return
				}
			}
			out.Write(sf.FormatProgress(utils.TruncateID(img.ID), "Download complete", nil))

			if parallel {
				errors <- nil
			}
		}

		if parallel {
			go downloadImage(image)
		} else {
			downloadImage(image)
		}
	}
	if parallel {
		var lastError error
		for i := 0; i < len(repoData.ImgList); i++ {
			if err := <-errors; err != nil {
				lastError = err
			}
		}
		if lastError != nil {
			return lastError
		}

	}
	for tag, id := range tagsList {
		if askedTag != "" && tag != askedTag {
			continue
		}
		if err := srv.daemon.Repositories().Set(localName, tag, id, true); err != nil {
			return err
		}
	}

	return nil
}

func (srv *Server) ImagePull(job *engine.Job) engine.Status {
	if n := len(job.Args); n != 1 && n != 2 {
		return job.Errorf("Usage: %s IMAGE [TAG]", job.Name)
	}
	var (
		localName   = job.Args[0]
		tag         string
		sf          = utils.NewStreamFormatter(job.GetenvBool("json"))
		authConfig  = &registry.AuthConfig{}
		metaHeaders map[string][]string
	)
	if len(job.Args) > 1 {
		tag = job.Args[1]
	}

	job.GetenvJson("authConfig", authConfig)
	job.GetenvJson("metaHeaders", &metaHeaders)

	c, err := srv.poolAdd("pull", localName+":"+tag)
	if err != nil {
		if c != nil {
			// Another pull of the same repository is already taking place; just wait for it to finish
			job.Stdout.Write(sf.FormatStatus("", "Repository %s already being pulled by another client. Waiting.", localName))
			<-c
			return engine.StatusOK
		}
		return job.Error(err)
	}
	defer srv.poolRemove("pull", localName+":"+tag)

	// Resolve the Repository name from fqn to endpoint + name
	hostname, remoteName, err := registry.ResolveRepositoryName(localName)
	if err != nil {
		return job.Error(err)
	}

	endpoint, err := registry.ExpandAndVerifyRegistryUrl(hostname)
	if err != nil {
		return job.Error(err)
	}

	r, err := registry.NewRegistry(authConfig, registry.HTTPRequestFactory(metaHeaders), endpoint, true)
	if err != nil {
		return job.Error(err)
	}

	if endpoint == registry.IndexServerAddress() {
		// If pull "index.docker.io/foo/bar", it's stored locally under "foo/bar"
		localName = remoteName
	}

	if err = srv.pullRepository(r, job.Stdout, localName, remoteName, tag, sf, job.GetenvBool("parallel")); err != nil {
		return job.Error(err)
	}

	return engine.StatusOK
}

// Retrieve the all the images to be uploaded in the correct order
func (srv *Server) getImageList(localRepo map[string]string, requestedTag string) ([]string, map[string][]string, error) {
	var (
		imageList   []string
		imagesSeen  map[string]bool     = make(map[string]bool)
		tagsByImage map[string][]string = make(map[string][]string)
	)

	for tag, id := range localRepo {
		if requestedTag != "" && requestedTag != tag {
			continue
		}
		var imageListForThisTag []string

		tagsByImage[id] = append(tagsByImage[id], tag)

		for img, err := srv.daemon.Graph().Get(id); img != nil; img, err = img.GetParent() {
			if err != nil {
				return nil, nil, err
			}

			if imagesSeen[img.ID] {
				// This image is already on the list, we can ignore it and all its parents
				break
			}

			imagesSeen[img.ID] = true
			imageListForThisTag = append(imageListForThisTag, img.ID)
		}

		// reverse the image list for this tag (so the "most"-parent image is first)
		for i, j := 0, len(imageListForThisTag)-1; i < j; i, j = i+1, j-1 {
			imageListForThisTag[i], imageListForThisTag[j] = imageListForThisTag[j], imageListForThisTag[i]
		}

		// append to main image list
		imageList = append(imageList, imageListForThisTag...)
	}
	if len(imageList) == 0 {
		return nil, nil, fmt.Errorf("No images found for the requested repository / tag")
	}
	utils.Debugf("Image list: %v", imageList)
	utils.Debugf("Tags by image: %v", tagsByImage)

	return imageList, tagsByImage, nil
}

func (srv *Server) pushRepository(r *registry.Registry, out io.Writer, localName, remoteName string, localRepo map[string]string, tag string, sf *utils.StreamFormatter) error {
	out = utils.NewWriteFlusher(out)
	utils.Debugf("Local repo: %s", localRepo)
	imgList, tagsByImage, err := srv.getImageList(localRepo, tag)
	if err != nil {
		return err
	}

	out.Write(sf.FormatStatus("", "Sending image list"))

	var (
		repoData   *registry.RepositoryData
		imageIndex []*registry.ImgData
	)

	for _, imgId := range imgList {
		if tags, exists := tagsByImage[imgId]; exists {
			// If an image has tags you must add an entry in the image index
			// for each tag
			for _, tag := range tags {
				imageIndex = append(imageIndex, &registry.ImgData{
					ID:  imgId,
					Tag: tag,
				})
			}
		} else {
			// If the image does not have a tag it still needs to be sent to the
			// registry with an empty tag so that it is accociated with the repository
			imageIndex = append(imageIndex, &registry.ImgData{
				ID:  imgId,
				Tag: "",
			})

		}
	}

	utils.Debugf("Preparing to push %s with the following images and tags\n", localRepo)
	for _, data := range imageIndex {
		utils.Debugf("Pushing ID: %s with Tag: %s\n", data.ID, data.Tag)
	}

	// Register all the images in a repository with the registry
	// If an image is not in this list it will not be associated with the repository
	repoData, err = r.PushImageJSONIndex(remoteName, imageIndex, false, nil)
	if err != nil {
		return err
	}

	nTag := 1
	if tag == "" {
		nTag = len(localRepo)
	}
	for _, ep := range repoData.Endpoints {
		out.Write(sf.FormatStatus("", "Pushing repository %s (%d tags)", localName, nTag))

		for _, imgId := range imgList {
			if r.LookupRemoteImage(imgId, ep, repoData.Tokens) {
				out.Write(sf.FormatStatus("", "Image %s already pushed, skipping", utils.TruncateID(imgId)))
			} else {
				if _, err := srv.pushImage(r, out, remoteName, imgId, ep, repoData.Tokens, sf); err != nil {
					// FIXME: Continue on error?
					return err
				}
			}

			for _, tag := range tagsByImage[imgId] {
				out.Write(sf.FormatStatus("", "Pushing tag for rev [%s] on {%s}", utils.TruncateID(imgId), ep+"repositories/"+remoteName+"/tags/"+tag))

				if err := r.PushRegistryTag(remoteName, imgId, tag, ep, repoData.Tokens); err != nil {
					return err
				}
			}
		}
	}

	if _, err := r.PushImageJSONIndex(remoteName, imageIndex, true, repoData.Endpoints); err != nil {
		return err
	}

	return nil
}

func (srv *Server) pushImage(r *registry.Registry, out io.Writer, remote, imgID, ep string, token []string, sf *utils.StreamFormatter) (checksum string, err error) {
	out = utils.NewWriteFlusher(out)
	jsonRaw, err := ioutil.ReadFile(path.Join(srv.daemon.Graph().Root, imgID, "json"))
	if err != nil {
		return "", fmt.Errorf("Cannot retrieve the path for {%s}: %s", imgID, err)
	}
	out.Write(sf.FormatProgress(utils.TruncateID(imgID), "Pushing", nil))

	imgData := &registry.ImgData{
		ID: imgID,
	}

	// Send the json
	if err := r.PushImageJSONRegistry(imgData, jsonRaw, ep, token); err != nil {
		if err == registry.ErrAlreadyExists {
			out.Write(sf.FormatProgress(utils.TruncateID(imgData.ID), "Image already pushed, skipping", nil))
			return "", nil
		}
		return "", err
	}

	layerData, err := srv.daemon.Graph().TempLayerArchive(imgID, archive.Uncompressed, sf, out)
	if err != nil {
		return "", fmt.Errorf("Failed to generate layer archive: %s", err)
	}
	defer os.RemoveAll(layerData.Name())

	// Send the layer
	utils.Debugf("rendered layer for %s of [%d] size", imgData.ID, layerData.Size)

	checksum, checksumPayload, err := r.PushImageLayerRegistry(imgData.ID, utils.ProgressReader(layerData, int(layerData.Size), out, sf, false, utils.TruncateID(imgData.ID), "Pushing"), ep, token, jsonRaw)
	if err != nil {
		return "", err
	}
	imgData.Checksum = checksum
	imgData.ChecksumPayload = checksumPayload
	// Send the checksum
	if err := r.PushImageChecksumRegistry(imgData, ep, token); err != nil {
		return "", err
	}

	out.Write(sf.FormatProgress(utils.TruncateID(imgData.ID), "Image successfully pushed", nil))
	return imgData.Checksum, nil
}

// FIXME: Allow to interrupt current push when new push of same image is done.
func (srv *Server) ImagePush(job *engine.Job) engine.Status {
	if n := len(job.Args); n != 1 {
		return job.Errorf("Usage: %s IMAGE", job.Name)
	}
	var (
		localName   = job.Args[0]
		sf          = utils.NewStreamFormatter(job.GetenvBool("json"))
		authConfig  = &registry.AuthConfig{}
		metaHeaders map[string][]string
	)

	tag := job.Getenv("tag")
	job.GetenvJson("authConfig", authConfig)
	job.GetenvJson("metaHeaders", &metaHeaders)
	if _, err := srv.poolAdd("push", localName); err != nil {
		return job.Error(err)
	}
	defer srv.poolRemove("push", localName)

	// Resolve the Repository name from fqn to endpoint + name
	hostname, remoteName, err := registry.ResolveRepositoryName(localName)
	if err != nil {
		return job.Error(err)
	}

	endpoint, err := registry.ExpandAndVerifyRegistryUrl(hostname)
	if err != nil {
		return job.Error(err)
	}

	img, err := srv.daemon.Graph().Get(localName)
	r, err2 := registry.NewRegistry(authConfig, registry.HTTPRequestFactory(metaHeaders), endpoint, false)
	if err2 != nil {
		return job.Error(err2)
	}

	if err != nil {
		reposLen := 1
		if tag == "" {
			reposLen = len(srv.daemon.Repositories().Repositories[localName])
		}
		job.Stdout.Write(sf.FormatStatus("", "The push refers to a repository [%s] (len: %d)", localName, reposLen))
		// If it fails, try to get the repository
		if localRepo, exists := srv.daemon.Repositories().Repositories[localName]; exists {
			if err := srv.pushRepository(r, job.Stdout, localName, remoteName, localRepo, tag, sf); err != nil {
				return job.Error(err)
			}
			return engine.StatusOK
		}
		return job.Error(err)
	}

	var token []string
	job.Stdout.Write(sf.FormatStatus("", "The push refers to an image: [%s]", localName))
	if _, err := srv.pushImage(r, job.Stdout, remoteName, img.ID, endpoint, token, sf); err != nil {
		return job.Error(err)
	}
	return engine.StatusOK
}

func (srv *Server) ImageImport(job *engine.Job) engine.Status {
	if n := len(job.Args); n != 2 && n != 3 {
		return job.Errorf("Usage: %s SRC REPO [TAG]", job.Name)
	}
	var (
		src     = job.Args[0]
		repo    = job.Args[1]
		tag     string
		sf      = utils.NewStreamFormatter(job.GetenvBool("json"))
		archive archive.ArchiveReader
		resp    *http.Response
	)
	if len(job.Args) > 2 {
		tag = job.Args[2]
	}

	if src == "-" {
		archive = job.Stdin
	} else {
		u, err := url.Parse(src)
		if err != nil {
			return job.Error(err)
		}
		if u.Scheme == "" {
			u.Scheme = "http"
			u.Host = src
			u.Path = ""
		}
		job.Stdout.Write(sf.FormatStatus("", "Downloading from %s", u))
		resp, err = utils.Download(u.String())
		if err != nil {
			return job.Error(err)
		}
		progressReader := utils.ProgressReader(resp.Body, int(resp.ContentLength), job.Stdout, sf, true, "", "Importing")
		defer progressReader.Close()
		archive = progressReader
	}
	img, err := srv.daemon.Graph().Create(archive, "", "", "Imported from "+src, "", nil, nil)
	if err != nil {
		return job.Error(err)
	}
	// Optionally register the image at REPO/TAG
	if repo != "" {
		if err := srv.daemon.Repositories().Set(repo, tag, img.ID, true); err != nil {
			return job.Error(err)
		}
	}
	job.Stdout.Write(sf.FormatStatus("", img.ID))
	return engine.StatusOK
}
func (srv *Server) DeleteImage(name string, imgs *engine.Table, first, force, noprune bool) error {
	var (
		repoName, tag string
		tags          = []string{}
		tagDeleted    bool
	)

	repoName, tag = parsers.ParseRepositoryTag(name)
	if tag == "" {
		tag = graph.DEFAULTTAG
	}

	img, err := srv.daemon.Repositories().LookupImage(name)
	if err != nil {
		if r, _ := srv.daemon.Repositories().Get(repoName); r != nil {
			return fmt.Errorf("No such image: %s:%s", repoName, tag)
		}
		return fmt.Errorf("No such image: %s", name)
	}

	if strings.Contains(img.ID, name) {
		repoName = ""
		tag = ""
	}

	byParents, err := srv.daemon.Graph().ByParent()
	if err != nil {
		return err
	}

	//If delete by id, see if the id belong only to one repository
	if repoName == "" {
		for _, repoAndTag := range srv.daemon.Repositories().ByID()[img.ID] {
			parsedRepo, parsedTag := parsers.ParseRepositoryTag(repoAndTag)
			if repoName == "" || repoName == parsedRepo {
				repoName = parsedRepo
				if parsedTag != "" {
					tags = append(tags, parsedTag)
				}
			} else if repoName != parsedRepo && !force {
				// the id belongs to multiple repos, like base:latest and user:test,
				// in that case return conflict
				return fmt.Errorf("Conflict, cannot delete image %s because it is tagged in multiple repositories, use -f to force", name)
			}
		}
	} else {
		tags = append(tags, tag)
	}

	if !first && len(tags) > 0 {
		return nil
	}

	//Untag the current image
	for _, tag := range tags {
		tagDeleted, err = srv.daemon.Repositories().Delete(repoName, tag)
		if err != nil {
			return err
		}
		if tagDeleted {
			out := &engine.Env{}
			out.Set("Untagged", repoName+":"+tag)
			imgs.Add(out)
			srv.LogEvent("untag", img.ID, "")
		}
	}
	tags = srv.daemon.Repositories().ByID()[img.ID]
	if (len(tags) <= 1 && repoName == "") || len(tags) == 0 {
		if len(byParents[img.ID]) == 0 {
			if err := srv.canDeleteImage(img.ID, force, tagDeleted); err != nil {
				return err
			}
			if err := srv.daemon.Repositories().DeleteAll(img.ID); err != nil {
				return err
			}
			if err := srv.daemon.Graph().Delete(img.ID); err != nil {
				return err
			}
			out := &engine.Env{}
			out.Set("Deleted", img.ID)
			imgs.Add(out)
			srv.LogEvent("delete", img.ID, "")
			if img.Parent != "" && !noprune {
				err := srv.DeleteImage(img.Parent, imgs, false, force, noprune)
				if first {
					return err
				}

			}

		}
	}
	return nil
}

func (srv *Server) ImageDelete(job *engine.Job) engine.Status {
	if n := len(job.Args); n != 1 {
		return job.Errorf("Usage: %s IMAGE", job.Name)
	}
	imgs := engine.NewTable("", 0)
	if err := srv.DeleteImage(job.Args[0], imgs, true, job.GetenvBool("force"), job.GetenvBool("noprune")); err != nil {
		return job.Error(err)
	}
	if len(imgs.Data) == 0 {
		return job.Errorf("Conflict, %s wasn't deleted", job.Args[0])
	}
	if _, err := imgs.WriteListTo(job.Stdout); err != nil {
		return job.Error(err)
	}
	return engine.StatusOK
}

func (srv *Server) canDeleteImage(imgID string, force, untagged bool) error {
	var message string
	if untagged {
		message = " (docker untagged the image)"
	}
	for _, container := range srv.daemon.List() {
		parent, err := srv.daemon.Repositories().LookupImage(container.Image)
		if err != nil {
			return err
		}

		if err := parent.WalkHistory(func(p *image.Image) error {
			if imgID == p.ID {
				if container.State.IsRunning() {
					if force {
						return fmt.Errorf("Conflict, cannot force delete %s because the running container %s is using it%s, stop it and retry", utils.TruncateID(imgID), utils.TruncateID(container.ID), message)
					}
					return fmt.Errorf("Conflict, cannot delete %s because the running container %s is using it%s, stop it and use -f to force", utils.TruncateID(imgID), utils.TruncateID(container.ID), message)
				} else if !force {
					return fmt.Errorf("Conflict, cannot delete %s because the container %s is using it%s, use -f to force", utils.TruncateID(imgID), utils.TruncateID(container.ID), message)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (srv *Server) poolAdd(kind, key string) (chan struct{}, error) {
	srv.Lock()
	defer srv.Unlock()

	if c, exists := srv.pullingPool[key]; exists {
		return c, fmt.Errorf("pull %s is already in progress", key)
	}
	if c, exists := srv.pushingPool[key]; exists {
		return c, fmt.Errorf("push %s is already in progress", key)
	}

	c := make(chan struct{})
	switch kind {
	case "pull":
		srv.pullingPool[key] = c
	case "push":
		srv.pushingPool[key] = c
	default:
		return nil, fmt.Errorf("Unknown pool type")
	}
	return c, nil
}

func (srv *Server) poolRemove(kind, key string) error {
	srv.Lock()
	defer srv.Unlock()
	switch kind {
	case "pull":
		if c, exists := srv.pullingPool[key]; exists {
			close(c)
			delete(srv.pullingPool, key)
		}
	case "push":
		if c, exists := srv.pushingPool[key]; exists {
			close(c)
			delete(srv.pushingPool, key)
		}
	default:
		return fmt.Errorf("Unknown pool type")
	}
	return nil
}
