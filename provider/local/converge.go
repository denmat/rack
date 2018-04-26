package local

import (
	"fmt"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/convox/rack/helpers"
	"github.com/convox/rack/manifest"
	"github.com/pkg/errors"
)

var convergeLock sync.Mutex

func (p *Provider) converge(app string) error {
	convergeLock.Lock()
	defer convergeLock.Unlock()

	log := p.logger("converge").Append("app=%q", app)

	m, r, err := helpers.AppManifest(p, app)
	if err != nil {
		return log.Error(err)
	}

	desired := []container{}

	var c []container

	// c, err = p.balancerContainers(m.Balancers, app, r.Id)
	// if err != nil {
	//   return errors.WithStack(log.Error(err))
	// }

	// desired = append(desired, c...)

	c, err = p.resourceContainers(m.Resources, app, r.Id)
	if err != nil {
		return errors.WithStack(log.Error(err))
	}

	desired = append(desired, c...)

	c, err = p.serviceContainers(m.Services, app, r.Id)
	if err != nil {
		return errors.WithStack(log.Error(err))
	}

	desired = append(desired, c...)

	current, err := containersByLabels(map[string]string{
		"convox.rack": p.Name,
		"convox.app":  app,
	})
	if err != nil {
		return errors.WithStack(log.Error(err))
	}

	needed := []container{}

	for _, c := range desired {
		found := false

		for _, d := range current {
			if reflect.DeepEqual(c.Labels, d.Labels) {
				found = true
				break
			}
		}

		if !found {
			needed = append(needed, c)
		}
	}

	for _, c := range needed {
		p.storageLogWrite(fmt.Sprintf("apps/%s/releases/%s/log", app, r.Id), []byte(fmt.Sprintf("starting: %s\n", c.Name)))

		id, err := p.containerStart(c, app, r.Id)
		if err != nil {
			return errors.WithStack(log.Error(err))
		}

		c.Id = id
	}

	if err := p.routeContainers(desired); err != nil {
		return errors.WithStack(log.Error(err))
	}

	return log.Success()
}

func (p *Provider) prune() error {
	convergeLock.Lock()
	defer convergeLock.Unlock()

	log := p.logger("prune")

	apps, err := p.AppList()
	if err != nil {
		return errors.WithStack(log.Error(err))
	}

	all, err := containersByLabels(map[string]string{
		"convox.rack": p.Name,
	})
	if err != nil {
		return errors.WithStack(log.Error(err))
	}

	for _, c := range all {
		found := false

		for _, a := range apps {
			if a.Name == c.Labels["convox.app"] {
				found = true
				break
			}
		}

		if !found {
			log.Successf("action=kill id=%s", c.Id)
			exec.Command("docker", "stop", c.Id).Run()
		}
	}

	return log.Success()
}

func resourcePort(kind string) (int, error) {
	switch kind {
	case "mysql":
		return 3306, nil
	case "postgres":
		return 5432, nil
	case "redis":
		return 6379, nil
	}

	return 0, fmt.Errorf("unknown resource type: %s", kind)
}

func resourceURL(app, kind, name string) (string, error) {
	switch kind {
	case "mysql":
		return fmt.Sprintf("mysql://mysql:password@%s.resource.%s.convox:3306/app", name, app), nil
	case "postgres":
		return fmt.Sprintf("postgres://postgres:password@%s.resource.%s.convox:5432/app?sslmode=disable", name, app), nil
	case "redis":
		return fmt.Sprintf("redis://%s.resource.%s.convox:6379/0", name, app), nil
	}

	return "", fmt.Errorf("unknown resource type: %s", kind)
}

func (p *Provider) resourceVolumes(app, kind, name string) ([]string, error) {
	switch kind {
	case "mysql":
		return []string{fmt.Sprintf("%s/%s/resource/%s:/var/lib/mysql", p.Volume, app, name)}, nil
	case "postgres":
		return []string{fmt.Sprintf("%s/%s/resource/%s:/var/lib/postgresql/data", p.Volume, app, name)}, nil
	case "redis":
		return []string{}, nil
	}

	return []string{}, fmt.Errorf("unknown resource type: %s", kind)
}

// func (p *Provider) balancerContainers(balancers manifest.Balancers, app, release string) ([]container, error) {
//   cs := []container{}

//   sys, err := p.SystemGet()
//   if err != nil {
//     return nil, err
//   }

//   for _, b := range balancers {
//     for _, e := range b.Endpoints {
//       command := []string{}

//       switch {
//       case e.Redirect != "":
//         command = []string{"balancer", e.Protocol, "redirect", e.Redirect}
//       case e.Target != "":
//         command = []string{"balancer", e.Protocol, "target", e.Target}
//       default:
//         return nil, fmt.Errorf("invalid balancer endpoint: %s:%s", b.Name, e.Port)
//       }

//       cs = append(cs, container{
//         Name:     fmt.Sprintf("%s.%s.balancer.%s", p.Name, app, b.Name),
//         Hostname: fmt.Sprintf("%s.balancer.%s.%s", b.Name, app, p.Name),
//         Port: containerPort{
//           Host:      443,
//           Container: 3000,
//         },
//         Memory:  64,
//         Image:   sys.Image,
//         Command: command,
//         Labels: map[string]string{
//           "convox.rack":    p.Name,
//           "convox.version": p.Version,
//           "convox.app":     app,
//           "convox.release": release,
//           "convox.type":    "balancer",
//           "convox.name":    b.Name,
//           "convox.port":    e.Port,
//         },
//       })
//     }
//   }

//   return cs, nil
// }

func (p *Provider) resourceContainers(resources manifest.Resources, app, release string) ([]container, error) {
	cs := []container{}

	for _, r := range resources {
		rp, err := resourcePort(r.Type)
		if err != nil {
			return nil, err
		}

		vs, err := p.resourceVolumes(app, r.Type, r.Name)
		if err != nil {
			return nil, err
		}

		hostname := fmt.Sprintf("%s.resource.%s", r.Name, app)

		cs = append(cs, container{
			Name:     fmt.Sprintf("%s.%s.resource.%s", p.Name, app, r.Name),
			Hostname: hostname,
			Targets: []containerTarget{
				containerTarget{FromScheme: "tcp", FromPort: rp, ToScheme: "tcp", ToPort: rp},
			},
			Image:   fmt.Sprintf("convox/%s", r.Type),
			Volumes: vs,
			Port:    rp,
			Labels: map[string]string{
				"convox.rack":     p.Name,
				"convox.version":  p.Version,
				"convox.app":      app,
				"convox.release":  release,
				"convox.type":     "resource",
				"convox.name":     r.Name,
				"convox.hostname": hostname,
				"convox.resource": r.Type,
			},
		})
	}

	return cs, nil
}

func (p *Provider) serviceContainers(services manifest.Services, app, release string) ([]container, error) {
	cs := []container{}

	m, r, err := helpers.ReleaseManifest(p, app, release)
	if err != nil {
		return nil, err
	}

	for _, s := range services {
		cmd := []string{}

		if c := strings.TrimSpace(s.Command); c != "" {
			cmd = append(cmd, "sh", "-c", c)
		}

		env, err := m.ServiceEnvironment(s.Name)
		if err != nil {
			return nil, err
		}

		// copy the map so we can hold on to it
		e := map[string]string{}

		for k, v := range env {
			e[k] = v
		}

		// add resources
		for _, sr := range s.Resources {
			for _, r := range m.Resources {
				if r.Name == sr {
					u, err := resourceURL(app, r.Type, r.Name)
					if err != nil {
						return nil, err
					}

					e[fmt.Sprintf("%s_URL", strings.ToUpper(sr))] = u
				}
			}
		}

		vv, err := p.serviceVolumes(app, s.Volumes)
		if err != nil {
			return nil, err
		}

		scheme := "tcp"

		switch s.Port.Scheme {
		case "https":
			scheme = "tls"
		}

		hostname := fmt.Sprintf("%s.%s", s.Name, app)

		for i := 1; i <= s.Scale.Count.Min; i++ {
			c := container{
				Hostname: hostname,
				Name:     fmt.Sprintf("%s.%s.service.%s.%d", p.Name, app, s.Name, i),
				Image:    fmt.Sprintf("%s/%s/%s:%s", p.Name, app, s.Name, r.Build),
				Command:  cmd,
				Env:      e,
				Memory:   s.Scale.Memory,
				Volumes:  vv,
				Port:     s.Port.Port,
				Labels: map[string]string{
					"convox.rack":     p.Name,
					"convox.version":  p.Version,
					"convox.app":      app,
					"convox.release":  release,
					"convox.type":     "service",
					"convox.name":     s.Name,
					"convox.hostname": hostname,
					"convox.service":  s.Name,
					"convox.index":    fmt.Sprintf("%d", i),
					"convox.port":     strconv.Itoa(s.Port.Port),
					"convox.scheme":   s.Port.Scheme,
				},
			}

			if c.Port != 0 {
				c.Targets = []containerTarget{
					containerTarget{FromScheme: "tcp", FromPort: 80, ToScheme: scheme, ToPort: s.Port.Port},
					containerTarget{FromScheme: "tls", FromPort: 443, ToScheme: scheme, ToPort: s.Port.Port},
				}
			}

			cs = append(cs, c)
		}
	}

	return cs, nil
}
